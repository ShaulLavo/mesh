package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// Target selects the instance a request goes to.
type Target struct {
	// Service is a service ID or name on this host.
	Service string
	// Instance selects an existing instance by ID. Empty selects the caller's
	// default instance, or the shared one.
	Instance string
	// New creates another isolated instance for the caller.
	New bool
}

// pick resolves target to an instance the caller may use, creating the
// caller's default or a new isolated instance when needed.
func (s *Supervisor) pick(ctx context.Context, caller Caller, target Target) (*instance, Definition, error) {
	s.mu.Lock()
	d, err := s.serviceLocked(target.Service)
	if err != nil {
		s.mu.Unlock()
		return nil, d, err
	}
	if d.Disabled {
		s.mu.Unlock()
		return nil, d, failure(codeUnavailable, false, "service %s is disabled", d.Name)
	}
	inst, err := s.findLocked(caller, d, target)
	if err != nil {
		s.mu.Unlock()
		return nil, d, err
	}
	if inst != nil {
		stale := inst.rec.Owner != "" && inst.rec.OwnerGrant != caller.Grant
		s.mu.Unlock()
		if stale {
			return inst, d, s.rebind(ctx, inst, caller)
		}
		return inst, d, nil
	}
	inst, err = s.newInstanceLocked(caller, d, !target.New)
	s.mu.Unlock()
	if err != nil {
		return nil, d, err
	}
	if err := s.persist(ctx, inst); err != nil {
		s.mu.Lock()
		delete(s.instances, inst.rec.ID)
		s.mu.Unlock()
		return nil, d, err
	}
	return inst, d, nil
}

// rebind hands an isolated instance to its caller's new grant incarnation.
// Whatever ran under the old grant is that grant's state, so it is stopped
// first rather than inherited.
func (s *Supervisor) rebind(ctx context.Context, inst *instance, caller Caller) error {
	if err := s.stopInstance(ctx, inst, "the owning device was approved again"); err != nil {
		return err
	}
	s.mu.Lock()
	inst.rec.OwnerGrant = caller.Grant
	inst.rec.Leases = slices.DeleteFunc(inst.rec.Leases, func(l Lease) bool { return l.Grant != caller.Grant })
	s.mu.Unlock()
	return s.persist(ctx, inst)
}

func (s *Supervisor) findLocked(caller Caller, d Definition, target Target) (*instance, error) {
	if target.Instance != "" {
		return s.findExplicitLocked(caller, d, target.Instance)
	}
	if target.New {
		return nil, nil
	}
	for _, inst := range s.instances {
		if inst.rec.ServiceID != d.ID {
			continue
		}
		if d.InstancePolicy == PolicyShared || (inst.rec.Default && inst.rec.Owner != "" && caller.owns(inst.rec.Owner)) {
			return inst, nil
		}
	}
	return nil, nil
}

func (s *Supervisor) findExplicitLocked(caller Caller, d Definition, raw string) (*instance, error) {
	instanceID, err := resolveInstanceTarget(s.cfg.HostID, d.ID, raw)
	if err != nil {
		return nil, err
	}
	inst := s.instances[instanceID]
	if inst == nil || inst.rec.ServiceID != d.ID {
		return nil, fmt.Errorf("instance %s of %s: %w", instanceID, d.Name, ErrNotFound)
	}
	if inst.rec.Owner != "" && !caller.owns(inst.rec.Owner) {
		return nil, fmt.Errorf("instance %s of %s: %w", instanceID, d.Name, ErrDenied)
	}
	return inst, nil
}

func (s *Supervisor) newInstanceLocked(caller Caller, d Definition, isDefault bool) (*instance, error) {
	if d.InstancePolicy == PolicyShared && !isDefault {
		return nil, invalid("service %s is shared and has exactly one instance", d.Name)
	}
	perService := 0
	for _, inst := range s.instances {
		if inst.rec.ServiceID == d.ID {
			perService++
		}
	}
	if len(s.instances) >= MaxHostInstances || perService >= MaxIsolatedInstances {
		return nil, fmt.Errorf("service %s: %w", d.Name, ErrLimit)
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	now := s.cfg.Now()
	rec := InstanceRecord{ID: id, ServiceID: d.ID, Default: isDefault, Revision: d.Revision, Intent: intentStop, State: StateStopped, CreatedAt: now, UpdatedAt: now}
	if d.InstancePolicy == PolicyIsolated {
		rec.Owner, rec.OwnerAccount, rec.OwnerGrant = caller.Key(), caller.Account, caller.Grant
	}
	inst := &instance{rec: rec}
	s.instances[id] = inst
	return inst, nil
}

// acquire returns a running generation at the service's current revision and
// counts one active use of it. Concurrent callers share one startup.
func (s *Supervisor) acquire(ctx context.Context, inst *instance) (*generation, error) {
	gen, _, _, err := s.acquireFor(ctx, inst, 0)
	return gen, err
}

// acquireFor returns a running generation at expectedRev (or any current
// non-disabled revision when expectedRev is 0), the current Definition, and
// whether an existing ready generation was reused rather than freshly started.
func (s *Supervisor) acquireFor(ctx context.Context, inst *instance, expectedRev int64) (*generation, Definition, bool, error) {
	s.mu.Lock()
	stops := inst.stops
	s.mu.Unlock()
	started := false
	for {
		gen, d, launched, err := s.acquireStep(ctx, inst, stops, expectedRev)
		if launched {
			started = true
		}
		if gen != nil || err != nil {
			return gen, d, !started, err
		}
	}
}

// acquireStep returns a usable generation, or waits for or performs one
// start and returns nothing so the caller looks again.
func (s *Supervisor) acquireStep(ctx context.Context, inst *instance, stops int, expectedRev int64) (*generation, Definition, bool, error) {
	s.mu.Lock()
	d, err := s.serviceLocked(inst.rec.ServiceID)
	if err != nil {
		s.mu.Unlock()
		return nil, Definition{}, false, err
	}
	if d.Disabled {
		s.mu.Unlock()
		return nil, Definition{}, false, failure(codeUnavailable, false, "service %s is disabled", d.Name)
	}
	if expectedRev != 0 && d.Revision != expectedRev {
		s.mu.Unlock()
		return nil, Definition{}, false, failure(codeUnavailable, true, "service %s changed before startup completed", d.Name)
	}
	// A stop while this caller waited wins; the caller does not restart
	// what was just stopped.
	if inst.stops != stops {
		s.mu.Unlock()
		return nil, Definition{}, false, failure(codeUnavailable, true, "instance %s was stopped", inst.rec.ID)
	}
	if gen := s.usableLocked(inst, d); gen != nil {
		gen.active++
		stopTimer(inst.idle)
		s.mu.Unlock()
		return gen, d, false, nil
	}
	if wait := inst.starting; wait != nil {
		s.mu.Unlock()
		return nil, Definition{}, false, waitFor(ctx, wait)
	}
	if err := startable(inst); err != nil {
		s.mu.Unlock()
		return nil, Definition{}, false, err
	}
	inst.starting = make(chan struct{})
	s.mu.Unlock()
	return nil, Definition{}, true, s.startDetached(ctx, inst, d)
}

// startDetached launches outside the caller's context, so a caller that
// leaves cannot abandon a worker half way through its launch, and waits for
// the outcome while the caller is still there.
func (s *Supervisor) startDetached(ctx context.Context, inst *instance, d Definition) error {
	done := make(chan error, 1)
	if !s.detach(func(lifetime context.Context) { done <- s.start(lifetime, inst, d) }) {
		s.mu.Lock()
		close(inst.starting)
		inst.starting = nil
		s.mu.Unlock()
		return failure(codeUnavailable, true, "the host is shutting down")
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("wait for MCP instance start: %w", ctx.Err())
	}
}

func waitFor(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for MCP instance start: %w", ctx.Err())
	}
}

// startable keeps a failed start from turning every call into a relaunch.
// The caller sees the classified failure; an explicit restart clears it.
func startable(inst *instance) error {
	if inst.rec.StopPending != nil {
		return failure(codeUnavailable, true, "instance %s: previous worker termination is unresolved", inst.rec.ID)
	}
	switch inst.rec.State {
	case StateUncertain:
		return failure(codeUnavailable, true, "instance %s: %s", inst.rec.ID, inst.rec.Reason)
	case StateIncompatible:
		return failure(codeIncompatible, false, "instance %s: %s", inst.rec.ID, inst.rec.Reason)
	case StateFailed:
		if inst.retry != nil {
			return failure(codeFailed, true, "instance %s: %s", inst.rec.ID, inst.rec.Reason)
		}
	}
	return nil
}

func (s *Supervisor) usableLocked(inst *instance, d Definition) *generation {
	gen := inst.current
	if gen == nil {
		return nil
	}
	if gen.upstream.Err() != nil || gen.revision != d.Revision {
		inst.current = nil
		s.drainLocked(inst, gen)
		return nil
	}
	return gen
}

func (s *Supervisor) start(ctx context.Context, inst *instance, d Definition) error {
	gen, err := s.launch(ctx, inst, d)
	if err != nil {
		s.mu.Lock()
		close(inst.starting)
		inst.starting = nil
		s.maybeReleaseEndpointsLocked(inst)
		s.scheduleRetryLocked(inst, d)
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	cur, ok := s.services[d.ID]
	changed := !ok || cur.Disabled || cur.Revision != d.Revision
	if !changed {
		inst.current = gen
		inst.backoff = 0
		inst.lastProbeState = ""
		inst.lastProbeReason = ""
		close(inst.starting)
		inst.starting = nil
		s.detach(func(ctx context.Context) { s.watch(ctx, inst, gen) })
		s.armIdleLocked(inst, d)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if stopErr := s.stopGeneration(ctx, gen); stopErr != nil && gen.ref.SessionID != "" {
		ref := gen.ref
		s.mu.Lock()
		inst.rec.StopPending = &ref
		inst.rec.State = StateUncertain
		inst.rec.Reason = "worker termination is unresolved: " + stopErr.Error()
		s.mu.Unlock()
		_ = s.persist(ctx, inst)
	}
	s.mu.Lock()
	close(inst.starting)
	inst.starting = nil
	s.maybeReleaseEndpointsLocked(inst)
	s.mu.Unlock()
	return failure(codeUnavailable, true, "service %s changed while starting", d.Name)
}

func (s *Supervisor) launch(ctx context.Context, inst *instance, d Definition) (*generation, error) {
	if !d.Owned() {
		launchCtx, cancel := context.WithTimeout(ctx, d.ReadyTimeout+stopTimeout)
		defer cancel()
		return s.connectExternal(launchCtx, inst, d)
	}
	argv, cwd, plainPipe := d.Argv, d.Cwd, false
	if d.Transport == TransportHTTP {
		argv, cwd, plainPipe = d.RunArgv, d.RunCwd, true
		if err := s.prepareManagedHTTPLaunch(ctx, inst, d); err != nil {
			return nil, err
		}
	}
	launchCtx, cancel := context.WithTimeout(ctx, d.ReadyTimeout+stopTimeout)
	defer cancel()
	env, err := launchEnvironment(d)
	if err != nil {
		return nil, s.settle(launchCtx, inst, StateFailed, err.Error(), nil)
	}
	if cwd == "" {
		cwd = s.cfg.HostDefaultCwd
	}
	if err := findCommand(argv, cwd, d.Env); err != nil {
		return nil, s.settle(launchCtx, inst, StateFailed, err.Error(), nil)
	}
	s.mu.Lock()
	cur, ok := s.services[d.ID]
	if !ok || cur.Disabled || cur.Revision != d.Revision {
		s.mu.Unlock()
		return nil, failure(codeUnavailable, true, "service %s changed before launch", d.Name)
	}
	inst.rec.Generation++
	inst.rec.Revision, inst.rec.Intent, inst.rec.State, inst.rec.Reason, inst.rec.ExitCode, inst.rec.Worker, inst.rec.StopPending = d.Revision, intentRun, StateStarting, "", nil, nil, nil
	key := LaunchKey{Service: d.ID, Instance: inst.rec.ID, Generation: inst.rec.Generation}
	s.mu.Unlock()
	// The intent is durable before any process exists.
	if err := s.persist(launchCtx, inst); err != nil {
		return nil, err
	}
	ref, err := s.cfg.Launcher.Launch(launchCtx, LaunchSpec{Key: key, Argv: argv, Cwd: cwd, Env: env, PlainPipe: plainPipe})
	if errors.Is(err, ErrLaunchUncertain) {
		return nil, s.settle(launchCtx, inst, StateUncertain, "worker launch outcome is unknown; no second worker starts until it settles: "+err.Error(), nil)
	}
	if err != nil {
		return nil, s.settle(launchCtx, inst, StateFailed, "worker did not start: "+err.Error(), nil)
	}
	s.mu.Lock()
	inst.rec.Worker = &ref
	s.mu.Unlock()
	if err := s.persist(launchCtx, inst); err != nil {
		// An unrecorded worker would outlive its instance; stop it now.
		_ = s.stopWorker(launchCtx, ref)
		return nil, err
	}
	return s.connect(launchCtx, inst, d, key, ref)
}

func (s *Supervisor) connect(ctx context.Context, inst *instance, d Definition, key LaunchKey, ref WorkerRef) (*generation, error) {
	if d.Transport == TransportHTTP {
		return s.connectManagedHTTP(ctx, inst, d, key, ref)
	}
	conn, err := s.cfg.Launcher.Dial(ctx, ref)
	if err != nil {
		return nil, s.failStart(ctx, inst, key, ref, "attach to worker: "+err.Error())
	}
	upstream := newStdioUpstream(ctx, conn, s.router)
	info, err := s.probe(ctx, upstream, d.ReadyTimeout)
	if err != nil {
		_ = upstream.Close()
		var f *Failure
		if errors.As(err, &f) && f.Code == codeIncompatible {
			_ = s.cfg.Launcher.Stop(ctx, ref)
			return nil, s.settle(ctx, inst, StateIncompatible, f.Err.Error(), nil)
		}
		return nil, s.failStart(ctx, inst, key, ref, err.Error())
	}
	s.mu.Lock()
	inst.rec.Worker = &ref
	s.mu.Unlock()
	if err := s.settle(ctx, inst, StateReady, "", nil); err != nil {
		_ = upstream.Close()
		_ = s.stopWorker(ctx, ref)
		return nil, err
	}
	gen := &generation{number: key.Generation, revision: d.Revision, ref: ref, upstream: upstream}
	gen.setDiscovery(serverInfoOf(info), capabilitiesOf(info))
	return gen, nil
}

// failStart classifies an early exit, such as a missing runtime, by the
// worker's exit status. If stopping a still-live worker fails, the reservation
// stays pending until recovery confirms termination.
func (s *Supervisor) failStart(ctx context.Context, inst *instance, key LaunchKey, ref WorkerRef, reason string) error {
	if status, code := s.awaitExit(ctx, key); status == WorkerEnded {
		return s.settle(ctx, inst, StateFailed, exitReason(code), code)
	}
	if err := s.stopWorker(ctx, ref); err != nil {
		s.mu.Lock()
		inst.rec.StopPending = &ref
		s.mu.Unlock()
		return s.settle(ctx, inst, StateUncertain, "worker termination is unresolved after failed start: "+err.Error(), nil)
	}
	return s.settle(ctx, inst, StateFailed, reason, nil)
}

// awaitExit gives a worker whose child just exited a moment to record the
// exit status, so the failure names it.
func (s *Supervisor) awaitExit(ctx context.Context, key LaunchKey) (WorkerStatus, *int) {
	ctx, cancel := context.WithTimeout(ctx, exitSettle)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, status, code, _ := s.cfg.Launcher.Locate(ctx, key)
		if status != WorkerLive {
			return status, code
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return status, nil
		}
	}
}

func exitReason(code *int) string {
	switch {
	case code == nil:
		return "the server exited before it was ready"
	case *code == exitNotFound:
		return "the server command or its runtime was not found (exit status 127)"
	case *code == exitNoPermit:
		return "the server command is not executable (exit status 126)"
	default:
		return fmt.Sprintf("the server exited before it was ready (exit status %d)", *code)
	}
}

// settle records a terminal start outcome and returns it as an error unless
// the instance is ready.
func (s *Supervisor) settle(ctx context.Context, inst *instance, state, reason string, code *int) error {
	s.mu.Lock()
	inst.rec.State, inst.rec.Reason, inst.rec.ExitCode = state, reason, code
	s.mu.Unlock()
	if err := s.persist(ctx, inst); err != nil {
		return err
	}
	switch state {
	case StateReady:
		return nil
	case StateIncompatible:
		return failure(codeIncompatible, false, "instance %s: %s", inst.rec.ID, reason)
	case StateUncertain:
		return failure(codeUnavailable, true, "instance %s: %s", inst.rec.ID, reason)
	default:
		return failure(codeFailed, true, "instance %s: %s", inst.rec.ID, reason)
	}
}

// probe sends server/discover with empty client capabilities and accepts
// only a well-formed modern result. A legacy server is never retried in a
// legacy mode.
func (s *Supervisor) probe(ctx context.Context, upstream Upstream, deadline time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	reply, err := s.hostCall(ctx, upstream, mcpwire.MethodDiscover, map[string]any{})
	return probeResult(reply, err)
}

// hostCall sends one of the host's own read-only requests, with empty client
// capabilities and a host-assigned ID, and returns its terminal reply.
func (s *Supervisor) hostCall(ctx context.Context, upstream Upstream, method string, params map[string]any) (mcpwire.Message, error) {
	id, _ := s.router.ids.request()
	params[metaKey] = map[string]any{mcpwire.VersionKey: mcpwire.Version, mcpwire.ClientCapsKey: map[string]any{}, mcpwire.ClientInfoKey: map[string]any{"name": "mesh", "version": "1"}}
	raw, err := encodeJSON(map[string]any{jsonrpcKey: jsonrpcVersion, "id": id, "method": method, paramsKey: params})
	if err != nil {
		return mcpwire.Message{}, err
	}
	request, err := reparse(raw)
	if err != nil {
		return mcpwire.Message{}, err
	}
	var reply mcpwire.Message
	err = upstream.Call(ctx, request, func(event mcpwire.Event) error {
		reply = event.Message
		return nil
	})
	if err != nil {
		return reply, fmt.Errorf("host %s: %w", method, err)
	}
	return reply, nil
}

func probeResult(reply mcpwire.Message, err error) (json.RawMessage, error) {
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) && httpErr.Status >= 500 {
		return nil, fmt.Errorf("server did not answer server/discover: %w", err)
	}
	if errors.Is(err, ErrOutcomeUnknown) && errors.Is(err, mcpwire.ErrPeerMessage) {
		return nil, failure(codeIncompatible, false, "server/discover reply is not a valid %s result: %w", mcpwire.Version, err)
	}
	if errors.Is(err, ErrProtocolFault) || errors.Is(err, ErrMalformedResponse) {
		return nil, failure(codeIncompatible, false, "server output is not %s MCP: %w", mcpwire.Version, err)
	}
	if err != nil {
		return nil, fmt.Errorf("server did not answer server/discover: %w", err)
	}
	if reply.ErrorObject() != nil {
		return nil, failure(codeIncompatible, false, "server refused server/discover; it does not speak MCP %s", mcpwire.Version)
	}
	return reply.Result(), nil
}

// watch notices a generation whose pipe faulted or whose process ended.
func (s *Supervisor) watch(ctx context.Context, inst *instance, gen *generation) {
	s.waitManagedExit(ctx, inst, gen)
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	status, code := WorkerAbsent, (*int)(nil)
	if gen.ref.SessionID != "" {
		status, code = s.awaitExit(ctx, LaunchKey{Service: inst.rec.ServiceID, Instance: inst.rec.ID, Generation: gen.number})
	}
	var stopErr error
	if status == WorkerLive {
		// Framing was lost while the process runs on: stop it.
		stopErr = s.stopWorker(ctx, gen.ref)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst.current != gen {
		return
	}
	inst.current = nil
	stopTimer(inst.idle)
	if inst.rec.Intent == intentStop {
		s.maybeReleaseEndpointsLocked(inst)
		return
	}
	if stopErr != nil && gen.ref.SessionID != "" {
		ref := gen.ref
		inst.rec.StopPending = &ref
		inst.rec.State = StateUncertain
		inst.rec.Reason = "worker termination is unresolved: " + stopErr.Error()
		s.detach(func(ctx context.Context) { _ = s.persist(ctx, inst) })
		return
	}
	s.maybeReleaseEndpointsLocked(inst)
	inst.rec.State, inst.rec.ExitCode = StateFailed, code
	inst.rec.Reason = faultReason(gen.upstream.Err(), status, code)
	s.detach(func(ctx context.Context) { _ = s.persist(ctx, inst) })
	if d, err := s.serviceLocked(inst.rec.ServiceID); err == nil {
		s.scheduleRetryLocked(inst, d)
	}
}

func faultReason(err error, status WorkerStatus, code *int) string {
	if status == WorkerEnded && code != nil {
		return fmt.Sprintf("the server exited with status %d", *code)
	}
	if status == WorkerEnded {
		return "the server exited"
	}
	if errors.Is(err, ErrProtocolFault) {
		return "the server broke MCP framing and was stopped"
	}
	return "the connection to the server's worker was lost"
}

// scheduleRetryLocked restarts kept services with backoff. An on-demand
// instance starts again on its next call once the backoff passes.
func (s *Supervisor) scheduleRetryLocked(inst *instance, d Definition) {
	if inst.rec.State != StateFailed || s.ctx.Err() != nil {
		return
	}
	inst.backoff = min(max(inst.backoff*2, minBackoff), maxBackoff)
	stopTimer(inst.retry)
	inst.retry = time.AfterFunc(inst.backoff, func() {
		s.mu.Lock()
		inst.retry = nil
		s.mu.Unlock()
		if d.Lifecycle == LifecycleKept {
			s.detach(func(ctx context.Context) { s.ensureKept(ctx, d.ID) })
		}
	})
}

func (s *Supervisor) release(inst *instance, gen *generation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gen.active--
	if gen.draining && gen.active == 0 {
		stop := gen.stopDrain
		if stop == nil {
			stop = func(ctx context.Context) { _ = s.stopGeneration(ctx, gen) }
		}
		s.detach(stop)
		return
	}
	if d, err := s.serviceLocked(inst.rec.ServiceID); err == nil {
		s.armIdleLocked(inst, d)
	}
}

// armIdleLocked starts the idle timer once active calls, acknowledged listens
// and leases reach zero. Idle expiry applies only to Mesh-owned processes.
func (s *Supervisor) armIdleLocked(inst *instance, d Definition) {
	gen := inst.current
	if gen == nil || !d.Owned() || gen.active > 0 || d.Lifecycle != LifecycleOnDemand || d.Idle <= 0 || s.leasedLocked(inst) {
		return
	}
	stopTimer(inst.idle)
	inst.idle = time.AfterFunc(d.Idle, func() {
		s.detach(func(ctx context.Context) { s.idleStop(ctx, inst, gen) })
	})
}

func (s *Supervisor) idleStop(ctx context.Context, inst *instance, gen *generation) {
	s.mu.Lock()
	eligible := inst.current == gen && gen.active == 0 && !s.leasedLocked(inst)
	s.mu.Unlock()
	if !eligible {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	_ = s.stopInstance(ctx, inst, idleReason)
}

// stopInstance records durable stop intent (and keeps the worker reservation
// in StopPending until termination is confirmed), then stops the exact current
// and any draining workers.
func (s *Supervisor) stopInstance(ctx context.Context, inst *instance, reason string) error {
	if err := s.waitClaim(ctx, inst); err != nil {
		return fmt.Errorf("stop instance %s: %w", inst.rec.ID, err)
	}
	defer s.finishStart(inst)
	s.mu.Lock()
	gen := inst.current
	inst.current = nil
	draining := slices.Clone(inst.draining)
	if reason != idleReason {
		inst.stops++
	}
	stopTimer(inst.idle)
	stopTimer(inst.retry)
	inst.retry = nil
	inst.lastProbeState = ""
	inst.lastProbeReason = ""
	ref := stopTargetLocked(gen, inst)
	inst.rec.Intent = intentStop
	if ref.SessionID != "" {
		refCopy := ref
		inst.rec.StopPending = &refCopy
		inst.rec.State = StateUncertain
		inst.rec.Reason = reason
	} else {
		inst.rec.StopPending = nil
		inst.rec.State = StateStopped
		inst.rec.Reason = reason
	}
	s.mu.Unlock()
	if err := s.persist(ctx, inst); err != nil {
		return err
	}
	if gen != nil {
		_ = gen.upstream.Close()
	}
	if err := s.finishDrainingStops(ctx, inst, draining); err != nil {
		return fmt.Errorf("stop instance %s: %w", inst.rec.ID, err)
	}
	if ref.SessionID != "" {
		if err := s.stopWorker(ctx, ref); err != nil {
			s.mu.Lock()
			refCopy := ref
			inst.rec.StopPending = &refCopy
			inst.rec.State = StateUncertain
			inst.rec.Reason = "worker termination is unresolved: " + err.Error()
			s.mu.Unlock()
			_ = s.persist(ctx, inst)
			return failure(codeUnavailable, true, "stop instance %s: %w", inst.rec.ID, err)
		}
		s.mu.Lock()
		inst.rec.StopPending = nil
		inst.rec.State = StateStopped
		inst.rec.Reason = reason
		s.mu.Unlock()
		return s.persist(ctx, inst)
	}
	return nil
}

func stopTargetLocked(gen *generation, inst *instance) WorkerRef {
	if gen != nil && gen.ref.SessionID != "" {
		return gen.ref
	}
	if inst.rec.StopPending != nil && inst.rec.StopPending.SessionID != "" {
		return *inst.rec.StopPending
	}
	if inst.rec.Worker != nil && inst.rec.Worker.SessionID != "" {
		return *inst.rec.Worker
	}
	return WorkerRef{}
}

func (s *Supervisor) finishDrainingStops(ctx context.Context, inst *instance, draining []*drainingGen) error {
	for _, dg := range draining {
		if dg.gen.stopDrain != nil {
			dg.gen.stopDrain(ctx)
		}
		select {
		case <-dg.done:
		case <-ctx.Done():
			return fmt.Errorf("wait for draining generation: %w", ctx.Err())
		}
		s.mu.Lock()
		err := dg.err
		s.mu.Unlock()
		if err == nil {
			continue
		}
		if retryErr := s.stopWorker(ctx, dg.gen.ref); retryErr != nil {
			return retryErr
		}
		s.mu.Lock()
		dg.err = nil
		inst.draining = slices.DeleteFunc(inst.draining, func(d *drainingGen) bool { return d == dg })
		s.mu.Unlock()
	}
	return nil
}

// idleReason marks an idle stop, which a newly arriving caller may undo.
const idleReason = "idle"

// waitClaim takes the start slot, waiting for its current holder.
func (s *Supervisor) waitClaim(ctx context.Context, inst *instance) error {
	for {
		s.mu.Lock()
		if inst.starting == nil {
			inst.starting = make(chan struct{})
			s.mu.Unlock()
			return nil
		}
		wait := inst.starting
		s.mu.Unlock()
		if err := waitFor(ctx, wait); err != nil {
			return err
		}
	}
}

// recover adopts the worker recorded for an instance's latest generation or
// finishes any pending stop recorded before a crash, using only the exact
// reservation, never a label.
func (s *Supervisor) recover(ctx context.Context, inst *instance) {
	d, err := s.service(inst.rec.ServiceID)
	if err != nil {
		return
	}
	if !d.Owned() {
		if inst.rec.Intent == intentRun {
			_ = s.settle(ctx, inst, StateStopped, "reconnects on its next use", nil)
		}
		return
	}
	if s.recoverStopIntent(ctx, inst) {
		return
	}
	if inst.rec.Generation == 0 || inst.rec.Intent != intentRun {
		return
	}
	key := LaunchKey{Service: inst.rec.ServiceID, Instance: inst.rec.ID, Generation: inst.rec.Generation}
	ref, status, code, err := s.cfg.Launcher.Locate(ctx, key)
	switch {
	case err != nil || status == WorkerUncertain:
		_ = s.settle(ctx, inst, StateUncertain, "could not determine whether the previous worker still runs", nil)
	case status == WorkerAbsent:
		_ = s.settle(ctx, inst, StateStopped, "no worker was started for the last launch", nil)
	case status == WorkerInterrupted:
		_ = s.settle(ctx, inst, StateInterrupted, "the host restarted; the server's process is gone", nil)
	case status == WorkerEnded:
		_ = s.settle(ctx, inst, StateFailed, faultReason(nil, status, code), code)
	case inst.rec.Revision != d.Revision:
		s.stopReplacedOnRecovery(ctx, inst, ref)
	default:
		s.adopt(ctx, inst, d, key, ref)
	}
}

// recoverStopIntent finishes any persisted StopPending or intentStop against
// the exact worker reservation. It returns true when recovery is complete (or
// still unresolved as StateUncertain) and adoption must not proceed.
func (s *Supervisor) recoverStopIntent(ctx context.Context, inst *instance) bool {
	s.mu.Lock()
	pending := inst.rec.StopPending
	intent := inst.rec.Intent
	gen := inst.rec.Generation
	reason := inst.rec.Reason
	s.mu.Unlock()
	if pending == nil && intent != intentStop {
		return false
	}
	if pending == nil && gen == 0 {
		return true
	}
	if !s.finishPendingStopOnRecovery(ctx, inst, pending, gen) {
		return true
	}
	s.mu.Lock()
	inst.rec.StopPending = nil
	inst.draining = nil
	s.mu.Unlock()
	if intent == intentStop {
		if reason == "" {
			reason = "stopped on request"
		}
		_ = s.settle(ctx, inst, StateStopped, reason, nil)
		return true
	}
	return false
}

func (s *Supervisor) finishPendingStopOnRecovery(ctx context.Context, inst *instance, pending *WorkerRef, gen int64) bool {
	key := LaunchKey{Service: inst.rec.ServiceID, Instance: inst.rec.ID, Generation: gen}
	var ref WorkerRef
	if gen > 0 {
		located, status, _, err := s.cfg.Launcher.Locate(ctx, key)
		switch {
		case err != nil || status == WorkerUncertain:
			_ = s.settle(ctx, inst, StateUncertain, "could not confirm whether the stopped worker exited", nil)
			return false
		case status == WorkerLive:
			ref = located
		}
	}
	if ref.SessionID == "" && pending != nil && pending.SessionID != "" {
		// Pending stop may belong to an older draining generation than gen.
		if inst.rec.Worker == nil || pending.SessionID != inst.rec.Worker.SessionID {
			ref = *pending
		}
	}
	if ref.SessionID == "" {
		return true
	}
	if err := s.stopWorker(ctx, ref); err != nil {
		s.mu.Lock()
		refCopy := ref
		inst.rec.StopPending = &refCopy
		s.mu.Unlock()
		_ = s.settle(ctx, inst, StateUncertain, "worker termination is unresolved: "+err.Error(), nil)
		return false
	}
	return true
}

func (s *Supervisor) stopReplacedOnRecovery(ctx context.Context, inst *instance, ref WorkerRef) {
	if err := s.stopWorker(ctx, ref); err != nil {
		s.mu.Lock()
		refCopy := ref
		inst.rec.StopPending = &refCopy
		s.mu.Unlock()
		_ = s.settle(ctx, inst, StateUncertain, "previous revision worker termination is unresolved: "+err.Error(), nil)
		return
	}
	s.mu.Lock()
	inst.rec.StopPending = nil
	s.mu.Unlock()
	_ = s.settle(ctx, inst, StateStopped, "replaced by a newer service revision", nil)
}

func (s *Supervisor) adopt(ctx context.Context, inst *instance, d Definition, key LaunchKey, ref WorkerRef) {
	gen, err := s.connect(ctx, inst, d, key, ref)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst.current = gen
	s.detach(func(ctx context.Context) { s.watch(ctx, inst, gen) })
	s.armIdleLocked(inst, d)
}

// startKept starts kept services in the background so a slow server never
// delays the daemon's own startup.
func (s *Supervisor) startKept() {
	s.mu.Lock()
	var kept []string
	for id, d := range s.services {
		if d.Lifecycle == LifecycleKept && !d.Disabled {
			kept = append(kept, id)
		}
	}
	s.mu.Unlock()
	for _, id := range kept {
		s.detach(func(ctx context.Context) { s.ensureKept(ctx, id) })
	}
}

func (s *Supervisor) ensureKept(ctx context.Context, serviceID string) {
	ctx, cancel := context.WithTimeout(ctx, maxReadyTimeout)
	defer cancel()
	inst, _, err := s.pick(ctx, Caller{Account: hostAccount}, Target{Service: serviceID})
	if err != nil {
		return
	}
	if gen, err := s.acquire(ctx, inst); err == nil {
		s.release(inst, gen)
	}
}
