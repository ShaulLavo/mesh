package mcphost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/storage"
)

const (
	managedHTTPProbePoll = 25 * time.Millisecond
	managedHTTPWatchPoll = 100 * time.Millisecond
)

// prepareManagedHTTPLaunch runs all pre-spawn invariants for a Mesh-managed
// HTTP service: credential file validation, predecessor drain completion,
// endpoint claim coordination across competing definitions, and preflight
// checks proving no process already listens on the target loopback port.
func (s *Supervisor) prepareManagedHTTPLaunch(ctx context.Context, inst *instance, d Definition) error {
	if _, err := secretHeaders(d); err != nil {
		return s.settle(ctx, inst, StateFailed, err.Error(), nil)
	}
	endpoint, err := parseHTTPEndpoint(d.URL)
	if err != nil {
		return s.settle(ctx, inst, StateFailed, err.Error(), nil)
	}
	guard, ok := s.cfg.Launcher.(ManagedHTTPGuard)
	if !ok {
		return s.settle(ctx, inst, StateFailed, "managed HTTP listener ownership verification is unavailable", nil)
	}
	drainCtx, drainCancel := context.WithTimeout(ctx, drainGrace+stopTimeout)
	defer drainCancel()
	if err := s.awaitDrainingManaged(drainCtx, inst); err != nil {
		return err
	}
	s.mu.Lock()
	cur, exists := s.services[d.ID]
	if !exists || cur.Disabled || cur.Revision != d.Revision {
		s.mu.Unlock()
		return failure(codeUnavailable, true, "service %s changed before launch", d.Name)
	}
	claimErr := s.claimEndpointsLocked(inst.rec.ID, endpoint.targets)
	s.mu.Unlock()
	if claimErr != nil {
		return s.settle(ctx, inst, StateFailed, claimErr.Error(), nil)
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, stopTimeout)
	defer checkCancel()
	if err := checkLoopbackTargetsFree(endpoint.targets); err != nil {
		return s.settle(ctx, inst, StateFailed, err.Error(), nil)
	}
	if err := guard.CheckPortFree(checkCtx, endpoint.targets); err != nil {
		return s.settle(ctx, inst, StateFailed, err.Error(), nil)
	}
	return nil
}

func checkLoopbackTargetsFree(targets []netip.AddrPort) error {
	for _, target := range targets {
		network := "tcp4"
		if target.Addr().Is6() {
			network = "tcp6"
		}
		ln, err := net.Listen(network, target.String())
		if err != nil {
			if target.Addr().Is6() && len(targets) > 1 && isIPv6Unavailable(err) {
				continue
			}
			return fmt.Errorf("managed HTTP endpoint %s is already occupied: %w", target, err)
		}
		_ = ln.Close()
	}
	return nil
}

func isIPv6Unavailable(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL)
}

// connectManagedHTTP attaches an HTTP upstream to a Mesh-managed plain pipe
// worker listening at d.URL. Every TCP connection is verified to belong to ref
// before any HTTP request or credential header is written.
func (s *Supervisor) connectManagedHTTP(ctx context.Context, inst *instance, d Definition, key LaunchKey, ref WorkerRef) (*generation, error) {
	headers, err := secretHeaders(d)
	if err != nil {
		return nil, s.failStart(ctx, inst, key, ref, err.Error())
	}
	guard, ok := s.cfg.Launcher.(ManagedHTTPGuard)
	if !ok {
		return nil, s.failStart(ctx, inst, key, ref, "managed HTTP listener ownership verification is unavailable")
	}
	gen := &generation{number: key.Generation, revision: d.Revision, ref: ref}
	budget := &byteBudget{limit: instanceQueueBytes, parent: s.router.budget}
	verify := func(dialCtx context.Context, target netip.AddrPort, dial func() (net.Conn, error)) (net.Conn, error) {
		return guard.VerifyManagedDial(dialCtx, ref, target, dial)
	}
	upstream, err := DialHTTP(HTTPUpstreamConfig{URL: d.URL, Headers: headers, ToolHeaders: gen.toolHeaders, VerifyDial: verify, budget: budget})
	if err != nil {
		return nil, s.failStart(ctx, inst, key, ref, "connect to "+d.URL+": "+err.Error())
	}
	gen.upstream = upstream
	discovered, err := s.probeManagedHTTP(ctx, inst, d, key, ref, upstream)
	if err != nil {
		_ = upstream.Close()
		return nil, err
	}
	gen.setDiscovery(serverInfoOf(discovered), capabilitiesOf(discovered))
	if err := s.refreshToolsIfAdvertised(ctx, gen, discovered); err != nil {
		_ = upstream.Close()
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
	return gen, nil
}

func (s *Supervisor) probeManagedHTTP(ctx context.Context, inst *instance, d Definition, key LaunchKey, ref WorkerRef, upstream Upstream) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, d.ReadyTimeout)
	defer cancel()
	deadline, _ := probeCtx.Deadline()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, s.failStart(ctx, inst, key, ref, "server did not answer server/discover before the ready timeout")
		}
		discovered, err := s.probe(probeCtx, upstream, remaining)
		if err == nil {
			return discovered, nil
		}
		retry, finalErr := s.handleManagedProbeError(ctx, probeCtx, inst, key, ref, err)
		if !retry {
			return nil, finalErr
		}
		if err := waitManagedRetry(probeCtx); err != nil {
			return nil, s.failStart(ctx, inst, key, ref, "server did not answer server/discover before the ready timeout")
		}
	}
}

func (s *Supervisor) handleManagedProbeError(ctx, probeCtx context.Context, inst *instance, key LaunchKey, ref WorkerRef, err error) (bool, error) {
	var f *Failure
	if errors.As(err, &f) && f.Code == codeIncompatible {
		if stopErr := s.stopWorker(ctx, ref); stopErr != nil {
			s.mu.Lock()
			refCopy := ref
			inst.rec.StopPending = &refCopy
			s.mu.Unlock()
			return false, s.settle(ctx, inst, StateUncertain, "incompatible worker termination is unresolved: "+stopErr.Error(), nil)
		}
		return false, s.settle(ctx, inst, StateIncompatible, f.Err.Error(), nil)
	}
	if errors.Is(err, ErrManagedListenerMismatch) {
		return false, s.failStart(ctx, inst, key, ref, err.Error())
	}
	if _, status, code, _ := s.cfg.Launcher.Locate(ctx, key); status == WorkerEnded {
		return false, s.settle(ctx, inst, StateFailed, exitReason(code), code)
	}
	if !errors.Is(err, ErrNotSent) || probeCtx.Err() != nil {
		return false, s.failStart(ctx, inst, key, ref, err.Error())
	}
	return true, nil
}

func waitManagedRetry(ctx context.Context) error {
	timer := time.NewTimer(managedHTTPProbePoll)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for managed HTTP retry: %w", ctx.Err())
	}
}

// waitManagedExit blocks until either gen.upstream closes, ctx is cancelled,
// or the managed HTTP worker exits on its own.
func (s *Supervisor) waitManagedExit(ctx context.Context, inst *instance, gen *generation) {
	if gen.ref.SessionID == "" {
		select {
		case <-gen.upstream.Done():
		case <-ctx.Done():
		}
		return
	}
	if _, isHTTP := gen.upstream.(*httpUpstream); !isHTTP {
		select {
		case <-gen.upstream.Done():
		case <-ctx.Done():
		}
		return
	}
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerGone := make(chan struct{})
	go func() {
		defer close(workerGone)
		s.pollWorkerExit(watchCtx, LaunchKey{Service: inst.rec.ServiceID, Instance: inst.rec.ID, Generation: gen.number}, gen.ref)
	}()
	select {
	case <-gen.upstream.Done():
	case <-workerGone:
		_ = gen.upstream.Close()
	case <-ctx.Done():
	}
	cancel()
	<-workerGone
}

func (s *Supervisor) pollWorkerExit(ctx context.Context, key LaunchKey, ref WorkerRef) {
	if waiter, ok := s.cfg.Launcher.(WorkerWaiter); ok {
		_ = waiter.Wait(ctx, ref)
		return
	}
	ticker := time.NewTicker(managedHTTPWatchPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, status, _, err := s.cfg.Launcher.Locate(ctx, key); err == nil && status != WorkerLive {
				return
			}
		}
	}
}

// awaitDrainingManaged waits until every replaced generation of inst has been
// confirmed exited. If termination failed or ctx expires first, it refuses to
// launch a replacement on the same port.
func (s *Supervisor) awaitDrainingManaged(ctx context.Context, inst *instance) error {
	for {
		s.mu.Lock()
		if inst.rec.StopPending != nil {
			reason := inst.rec.Reason
			s.mu.Unlock()
			return failure(codeUnavailable, true, "instance %s: previous worker termination is unresolved (%s)", inst.rec.ID, reason)
		}
		pending := slices.Clone(inst.draining)
		s.mu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		for _, dg := range pending {
			select {
			case <-dg.done:
			case <-ctx.Done():
				return failure(codeUnavailable, true, "instance %s: previous generation is still draining", inst.rec.ID)
			}
			s.mu.Lock()
			err := dg.err
			s.mu.Unlock()
			if err != nil {
				return failure(codeUnavailable, true, "instance %s: previous generation did not stop: %v", inst.rec.ID, err)
			}
		}
	}
}

// SetDisabled enables or disables a service definition at a new revision.
// Disabling stops every active instance or connection of that service.
func (s *Supervisor) SetDisabled(ctx context.Context, ref string, disabled bool) (CatalogService, error) {
	admin := Caller{Account: hostAccount, Admin: true}
	s.mu.Lock()
	serviceID, stopping, unchanged, err := s.toggleServiceLocked(ctx, ref, disabled)
	if unchanged != nil || err != nil {
		s.mu.Unlock()
		if err != nil {
			return CatalogService{}, err
		}
		return *unchanged, nil
	}
	s.mu.Unlock()
	for _, inst := range stopping {
		if err := s.stopInstance(ctx, inst, "service disabled"); err != nil {
			return CatalogService{}, err
		}
	}
	return s.CatalogService(admin, serviceID)
}

func (s *Supervisor) toggleServiceLocked(ctx context.Context, ref string, disabled bool) (string, []*instance, *CatalogService, error) {
	d, err := s.serviceLocked(ref)
	if err != nil {
		return "", nil, nil, err
	}
	if d.Disabled == disabled {
		view := s.catalogLocked(Caller{Account: hostAccount, Admin: true}, d)
		return d.ID, nil, &view, nil
	}
	updated := d
	updated.Disabled, updated.Revision, updated.UpdatedAt = disabled, d.Revision+1, s.cfg.Now()
	row, err := serviceRow(updated)
	if err != nil {
		return "", nil, nil, err
	}
	if err := s.cfg.Store.PutMCPService(ctx, row, d.Revision); err != nil {
		if errors.Is(err, storage.ErrMCPRevision) {
			return "", nil, nil, fmt.Errorf("update %s: %w: %w", d.Name, ErrConflict, err)
		}
		return "", nil, nil, fmt.Errorf("update %s: %w", d.Name, err)
	}
	s.services[updated.ID] = updated
	if !disabled && updated.Lifecycle == LifecycleKept {
		s.detach(func(ctx context.Context) { s.ensureKept(ctx, updated.ID) })
		return updated.ID, nil, nil, nil
	}
	if !disabled {
		return updated.ID, nil, nil, nil
	}
	var stopping []*instance
	for _, inst := range s.instances {
		if inst.rec.ServiceID == updated.ID {
			stopping = append(stopping, inst)
		}
	}
	return updated.ID, stopping, nil, nil
}

// Probe performs a read-only server/discover check (and HTTP tool schema
// validation) for an external HTTP or owned service without disrupting active
// calls. It returns the host-projected, allowlisted CatalogInstance.
func (s *Supervisor) Probe(ctx context.Context, caller Caller, target Target) (CatalogInstance, error) {
	if caller.Admin && target.Instance == "" {
		caller.Admin = false
	}
	inst, d, err := s.pick(ctx, caller, target)
	if err != nil {
		return CatalogInstance{}, err
	}
	s.mu.Lock()
	needsStart := inst.current == nil || inst.current.revision != d.Revision || inst.current.upstream.Err() != nil
	s.mu.Unlock()
	if needsStart {
		s.resettle(ctx, inst)
	}
	gen, cur, reused, err := s.acquireFor(ctx, inst, d.Revision)
	if err != nil {
		return s.catalogViewOf(caller, inst), err
	}
	defer s.release(inst, gen)
	if !reused {
		// acquireFor just probed server/discover and refreshed HTTP tool schemas.
		return s.catalogViewOf(caller, inst), nil
	}
	return s.reprobeWarm(ctx, caller, inst, cur, gen)
}

func (s *Supervisor) catalogViewOf(caller Caller, inst *instance) CatalogInstance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.catalogInstanceLocked(caller, inst)
}

// reprobeWarm checks protocol health on an already-running generation without
// closing its upstream, stopping its worker, or interrupting in-flight calls.
func (s *Supervisor) reprobeWarm(ctx context.Context, caller Caller, inst *instance, d Definition, gen *generation) (CatalogInstance, error) {
	discovered, err := s.probe(ctx, gen.upstream, d.ReadyTimeout)
	if err == nil && d.Transport == TransportHTTP {
		err = s.refreshToolsIfAdvertised(ctx, gen, discovered)
	}
	if err == nil {
		gen.setDiscovery(serverInfoOf(discovered), capabilitiesOf(discovered))
		s.mu.Lock()
		inst.lastProbeState = ""
		inst.lastProbeReason = ""
		view := s.catalogInstanceLocked(caller, inst)
		s.mu.Unlock()
		return view, nil
	}
	if ctx.Err() != nil {
		return s.catalogViewOf(caller, inst), fmt.Errorf("probe %s: %w", d.Name, err)
	}
	probeState, probeErr := classifyWarmProbeError(inst.rec.ID, err)
	s.mu.Lock()
	inst.lastProbeState = probeState
	inst.lastProbeReason = err.Error()
	view := s.catalogInstanceLocked(caller, inst)
	s.mu.Unlock()
	view.State = probeState
	view.Reason = err.Error()
	return view, probeErr
}

func classifyWarmProbeError(instanceID string, err error) (string, error) {
	var f *Failure
	if errors.As(err, &f) && f.Code == codeIncompatible {
		return StateIncompatible, failure(codeIncompatible, false, "instance %s: %s", instanceID, f.Err.Error())
	}
	return StateFailed, failure(codeFailed, true, "instance %s: %s", instanceID, err.Error())
}
