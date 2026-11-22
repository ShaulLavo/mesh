package mcphost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/storage"
)

const (
	managedHTTPProbePoll  = 25 * time.Millisecond
	managedHTTPWatchPoll  = 100 * time.Millisecond
	managedHTTPDrainPause = 5 * time.Second
)

// connectManagedHTTP attaches an HTTP upstream to a Mesh-managed plain pipe
// worker listening at d.URL. Because the process may need a moment after spawn
// to bind its loopback port, connection refusals are retried until ReadyTimeout
// unless the worker exits or answers with an incompatible protocol.
func (s *Supervisor) connectManagedHTTP(ctx context.Context, inst *instance, d Definition, key LaunchKey, ref WorkerRef) (*generation, error) {
	headers, err := secretHeaders(d)
	if err != nil {
		return nil, s.failStart(ctx, inst, key, ref, err.Error())
	}
	gen := &generation{number: key.Generation, revision: d.Revision, ref: ref}
	budget := &byteBudget{limit: instanceQueueBytes, parent: s.router.budget}
	upstream, err := DialHTTP(HTTPUpstreamConfig{URL: d.URL, Headers: headers, ToolHeaders: gen.toolHeaders, budget: budget})
	if err != nil {
		return nil, s.failStart(ctx, inst, key, ref, "connect to "+d.URL+": "+err.Error())
	}
	gen.upstream = upstream
	discovered, err := s.probeManagedHTTP(ctx, inst, d, key, ref, upstream)
	if err != nil {
		_ = upstream.Close()
		return nil, err
	}
	gen.serverInfo = serverInfoOf(discovered)
	gen.capabilities = capabilitiesOf(discovered)
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
		var f *Failure
		if errors.As(err, &f) && f.Code == codeIncompatible {
			_ = s.stopWorker(ctx, ref)
			return nil, s.settle(ctx, inst, StateIncompatible, f.Err.Error(), nil)
		}
		if _, status, code, _ := s.cfg.Launcher.Locate(ctx, key); status == WorkerEnded {
			return nil, s.settle(ctx, inst, StateFailed, exitReason(code), code)
		}
		if !errors.Is(err, ErrNotSent) || probeCtx.Err() != nil {
			return nil, s.failStart(ctx, inst, key, ref, err.Error())
		}
		if err := waitManagedRetry(probeCtx); err != nil {
			return nil, s.failStart(ctx, inst, key, ref, "server did not answer server/discover before the ready timeout")
		}
	}
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

// awaitDrainingManaged waits for a replaced generation of a managed HTTP
// instance to release its loopback port before launching a new worker.
func (s *Supervisor) awaitDrainingManaged(ctx context.Context, inst *instance) {
	s.mu.Lock()
	done := inst.drainingDone
	s.mu.Unlock()
	if done == nil {
		return
	}
	waitCtx, cancel := context.WithTimeout(ctx, managedHTTPDrainPause)
	defer cancel()
	select {
	case <-done:
	case <-waitCtx.Done():
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
// validation) for an external HTTP or owned service without disrupting calls.
func (s *Supervisor) Probe(ctx context.Context, caller Caller, target Target) (InstanceView, error) {
	if caller.Admin && target.Instance == "" {
		caller.Admin = false
	}
	inst, d, err := s.pick(ctx, caller, target)
	if err != nil {
		return InstanceView{}, err
	}
	s.mu.Lock()
	alreadyReady := inst.current != nil && inst.current.revision == d.Revision && inst.current.upstream.Err() == nil
	s.mu.Unlock()
	if !alreadyReady {
		s.resettle(ctx, inst)
	}
	gen, err := s.acquire(ctx, inst)
	if err != nil {
		return s.viewOf(caller, inst), err
	}
	defer s.release(inst, gen)
	if !alreadyReady {
		// acquire just probed server/discover and refreshed HTTP tool schemas.
		return s.viewOf(caller, inst), nil
	}
	if err := s.reprobeWarm(ctx, inst, d, gen); err != nil {
		return s.viewOf(caller, inst), err
	}
	return s.viewOf(caller, inst), nil
}

func (s *Supervisor) reprobeWarm(ctx context.Context, inst *instance, d Definition, gen *generation) error {
	discovered, err := s.probe(ctx, gen.upstream, d.ReadyTimeout)
	if err != nil {
		return s.failWarmProbe(ctx, inst, d, gen, err)
	}
	if d.Transport == TransportHTTP {
		if err := s.refreshToolsIfAdvertised(ctx, gen, discovered); err != nil {
			return s.failWarmProbe(ctx, inst, d, gen, err)
		}
	}
	s.mu.Lock()
	gen.serverInfo = serverInfoOf(discovered)
	gen.capabilities = capabilitiesOf(discovered)
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) failWarmProbe(ctx context.Context, inst *instance, d Definition, gen *generation, err error) error {
	s.mu.Lock()
	if inst.current == gen {
		inst.current = nil
	}
	s.mu.Unlock()
	_ = gen.upstream.Close()
	if d.Owned() {
		_ = s.stopWorker(ctx, gen.ref)
	}
	return s.failExternal(ctx, inst, err)
}
