package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if inst != nil || err != nil {
		s.mu.Unlock()
		return inst, d, err
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

func (s *Supervisor) findLocked(caller Caller, d Definition, target Target) (*instance, error) {
	if target.Instance != "" {
		inst := s.instances[target.Instance]
		if inst == nil || inst.rec.ServiceID != d.ID {
			return nil, fmt.Errorf("instance %s of %s: %w", target.Instance, d.Name, ErrNotFound)
		}
		if inst.rec.Owner != "" && inst.rec.Owner != caller.Key() {
			return nil, fmt.Errorf("instance %s of %s: %w", target.Instance, d.Name, ErrDenied)
		}
		return inst, nil
	}
	if target.New {
		return nil, nil
	}
	for _, inst := range s.instances {
		if inst.rec.ServiceID != d.ID {
			continue
		}
		if d.InstancePolicy == PolicyShared || (inst.rec.Default && inst.rec.Owner == caller.Key()) {
			return inst, nil
		}
	}
	return nil, nil
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
		rec.Owner, rec.OwnerAccount = caller.Key(), caller.Account
	}
	inst := &instance{rec: rec}
	s.instances[id] = inst
	return inst, nil
}

// acquire returns a running generation at the service's current revision and
// counts one active use of it. Concurrent callers share one startup.
func (s *Supervisor) acquire(ctx context.Context, inst *instance) (*generation, error) {
	for {
		s.mu.Lock()
		d, err := s.serviceLocked(inst.rec.ServiceID)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if gen := s.usableLocked(inst, d); gen != nil {
			gen.active++
			stopTimer(inst.idle)
			s.mu.Unlock()
			return gen, nil
		}
		if inst.starting != nil {
			wait := inst.starting
			s.mu.Unlock()
			if err := waitFor(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		if err := startable(inst); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		inst.starting = make(chan struct{})
		s.mu.Unlock()
		if err := s.start(inst, d); err != nil {
			return nil, err
		}
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

// start runs one launch without the caller's context: a caller leaving
// cannot abandon a worker half way through its launch.
func (s *Supervisor) start(inst *instance, d Definition) error {
	gen, err := s.launch(inst, d)
	s.mu.Lock()
	defer s.mu.Unlock()
	close(inst.starting)
	inst.starting = nil
	if err != nil {
		s.scheduleRetryLocked(inst, d)
		return err
	}
	inst.current = gen
	inst.backoff = 0
	s.wg.Add(1)
	go s.watch(inst, gen)
	s.armIdleLocked(inst, d)
	return nil
}

func (s *Supervisor) launch(inst *instance, d Definition) (*generation, error) {
	ctx, cancel := context.WithTimeout(s.ctx, d.ReadyTimeout+stopTimeout)
	defer cancel()
	env, err := launchEnvironment(d)
	if err != nil {
		return nil, s.settle(ctx, inst, StateFailed, err.Error(), nil)
	}
	s.mu.Lock()
	inst.rec.Generation++
	inst.rec.Revision, inst.rec.Intent, inst.rec.State, inst.rec.Reason, inst.rec.ExitCode, inst.rec.Worker = d.Revision, intentRun, StateStarting, "", nil, nil
	key := LaunchKey{Service: d.ID, Instance: inst.rec.ID, Generation: inst.rec.Generation}
	s.mu.Unlock()
	// The intent is durable before any process exists.
	if err := s.persist(ctx, inst); err != nil {
		return nil, err
	}
	cwd := d.Cwd
	if cwd == "" {
		cwd = s.cfg.HostDefaultCwd
	}
	ref, err := s.cfg.Launcher.Launch(ctx, LaunchSpec{Key: key, Argv: d.Argv, Cwd: cwd, Env: env})
	if errors.Is(err, ErrLaunchUncertain) {
		return nil, s.settle(ctx, inst, StateUncertain, "worker launch outcome is unknown; no second worker starts until it settles: "+err.Error(), nil)
	}
	if err != nil {
		return nil, s.settle(ctx, inst, StateFailed, "worker did not start: "+err.Error(), nil)
	}
	s.mu.Lock()
	inst.rec.Worker = &ref
	s.mu.Unlock()
	if err := s.persist(ctx, inst); err != nil {
		return nil, err
	}
	return s.connect(ctx, inst, d, key, ref)
}

func (s *Supervisor) connect(ctx context.Context, inst *instance, d Definition, key LaunchKey, ref WorkerRef) (*generation, error) {
	conn, err := s.cfg.Launcher.Dial(ctx, ref)
	if err != nil {
		return nil, s.failStart(ctx, inst, key, ref, "attach to worker: "+err.Error())
	}
	upstream := newStdioUpstream(conn, s.router.ids)
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
	if err := s.settle(ctx, inst, StateReady, "", nil); err != nil {
		_ = upstream.Close()
		return nil, err
	}
	return &generation{number: key.Generation, revision: d.Revision, ref: ref, upstream: upstream, serverInfo: info}, nil
}

// failStart classifies an early exit, such as a missing runtime, by the
// worker's exit status.
func (s *Supervisor) failStart(ctx context.Context, inst *instance, key LaunchKey, ref WorkerRef, reason string) error {
	if status, code := s.awaitExit(ctx, key); status == WorkerEnded {
		return s.settle(ctx, inst, StateFailed, exitReason(code), code)
	}
	_ = s.cfg.Launcher.Stop(ctx, ref)
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
func (s *Supervisor) probe(ctx context.Context, upstream *StdioUpstream, deadline time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	id, _ := s.router.ids.request()
	raw, err := encodeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": mcpwire.MethodDiscover, "params": map[string]any{
		"_meta": map[string]any{mcpwire.VersionKey: mcpwire.Version, mcpwire.ClientCapsKey: map[string]any{}, mcpwire.ClientInfoKey: map[string]any{"name": "mesh", "version": "probe"}},
	}})
	if err != nil {
		return nil, err
	}
	request, err := reparse(raw)
	if err != nil {
		return nil, err
	}
	var reply mcpwire.Message
	err = upstream.Call(ctx, request, func(event mcpwire.Event) error {
		reply = event.Message
		return nil
	})
	return probeResult(reply, err)
}

func probeResult(reply mcpwire.Message, err error) (json.RawMessage, error) {
	if errors.Is(err, ErrOutcomeUnknown) && errors.Is(err, mcpwire.ErrPeerMessage) {
		return nil, failure(codeIncompatible, false, "server/discover reply is not a valid %s result: %w", mcpwire.Version, err)
	}
	if errors.Is(err, ErrProtocolFault) {
		return nil, failure(codeIncompatible, false, "server output is not %s MCP: %w", mcpwire.Version, err)
	}
	if err != nil {
		return nil, fmt.Errorf("server did not answer server/discover: %w", err)
	}
	if reply.ErrorObject() != nil {
		return nil, failure(codeIncompatible, false, "server refused server/discover; it does not speak MCP %s", mcpwire.Version)
	}
	return member(reply.Result(), "_meta", mcpwire.ServerInfoKey), nil
}

// watch notices a generation whose pipe faulted or whose process ended.
func (s *Supervisor) watch(inst *instance, gen *generation) {
	defer s.wg.Done()
	select {
	case <-gen.upstream.Done():
	case <-s.ctx.Done():
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, stopTimeout)
	defer cancel()
	_, status, code, _ := s.cfg.Launcher.Locate(ctx, LaunchKey{Service: inst.rec.ServiceID, Instance: inst.rec.ID, Generation: gen.number})
	if status == WorkerLive {
		// Framing was lost while the process runs on: stop it.
		_ = s.cfg.Launcher.Stop(ctx, gen.ref)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst.current != gen {
		return
	}
	inst.current = nil
	stopTimer(inst.idle)
	if inst.rec.Intent == intentStop {
		return
	}
	inst.rec.State, inst.rec.ExitCode = StateFailed, code
	inst.rec.Reason = faultReason(gen.upstream.Err(), status, code)
	go func() { _ = s.persist(s.ctx, inst) }()
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
			s.ensureKept(d.ID)
		}
	})
}

func (s *Supervisor) release(inst *instance, gen *generation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gen.active--
	if gen.draining && gen.active == 0 {
		go s.stopGeneration(gen)
		return
	}
	if d, err := s.serviceLocked(inst.rec.ServiceID); err == nil {
		s.armIdleLocked(inst, d)
	}
}

// armIdleLocked starts the idle timer once active calls, acknowledged listens
// and leases reach zero.
func (s *Supervisor) armIdleLocked(inst *instance, d Definition) {
	gen := inst.current
	if gen == nil || gen.active > 0 || d.Lifecycle != LifecycleOnDemand || d.Idle <= 0 || s.leasedLocked(inst) {
		return
	}
	stopTimer(inst.idle)
	inst.idle = time.AfterFunc(d.Idle, func() { s.idleStop(inst, gen) })
}

func (s *Supervisor) idleStop(inst *instance, gen *generation) {
	s.mu.Lock()
	eligible := inst.current == gen && gen.active == 0 && !s.leasedLocked(inst)
	s.mu.Unlock()
	if !eligible {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, stopTimeout)
	defer cancel()
	_ = s.stopInstance(ctx, inst, "idle")
}

// stopInstance records stop intent, then stops the exact current worker.
func (s *Supervisor) stopInstance(ctx context.Context, inst *instance, reason string) error {
	s.mu.Lock()
	gen := inst.current
	inst.current = nil
	stopTimer(inst.idle)
	stopTimer(inst.retry)
	inst.retry = nil
	inst.rec.Intent, inst.rec.State, inst.rec.Reason = intentStop, StateStopped, reason
	s.mu.Unlock()
	if err := s.persist(ctx, inst); err != nil {
		return err
	}
	if gen != nil {
		_ = gen.upstream.Close()
		if err := s.cfg.Launcher.Stop(ctx, gen.ref); err != nil {
			return fmt.Errorf("stop instance %s: %w", inst.rec.ID, err)
		}
	}
	return nil
}

// recover adopts the worker recorded for an instance's latest generation,
// using only the exact reservation, never a label.
func (s *Supervisor) recover(ctx context.Context, inst *instance) {
	if inst.rec.Generation == 0 || inst.rec.Intent != intentRun {
		return
	}
	d, err := s.service(inst.rec.ServiceID)
	if err != nil {
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
		_ = s.cfg.Launcher.Stop(ctx, ref)
		_ = s.settle(ctx, inst, StateStopped, "replaced by a newer service revision", nil)
	default:
		s.adopt(ctx, inst, d, key, ref)
	}
}

func (s *Supervisor) adopt(ctx context.Context, inst *instance, d Definition, key LaunchKey, ref WorkerRef) {
	gen, err := s.connect(ctx, inst, d, key, ref)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst.current = gen
	s.wg.Add(1)
	go s.watch(inst, gen)
	s.armIdleLocked(inst, d)
}

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
		s.ensureKept(id)
	}
}

func (s *Supervisor) ensureKept(serviceID string) {
	ctx, cancel := context.WithTimeout(s.ctx, maxReadyTimeout)
	defer cancel()
	inst, _, err := s.pick(ctx, Caller{Account: "host"}, Target{Service: serviceID})
	if err != nil {
		return
	}
	if gen, err := s.acquire(ctx, inst); err == nil {
		s.release(inst, gen)
	}
}
