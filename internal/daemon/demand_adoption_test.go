package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

var errAdoptionLookup = errors.New("catalog lookup unavailable")

type adoptionLookupGate struct {
	ctx     context.Context
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
	release sync.Once
}

func (g *adoptionLookupGate) unblock() { g.release.Do(func() { close(g.resume) }) }

type demandAdoptionSessions struct {
	*fakeDemandSessions
	mu     sync.Mutex
	err    error
	id     string
	found  bool
	labels []string
	gate   *adoptionLookupGate
}

func (s *demandAdoptionSessions) findLabelled(ctx context.Context, label string) (string, bool, error) {
	s.mu.Lock()
	s.labels = append(s.labels, label)
	err, id, found, gate := s.err, s.id, s.found, s.gate
	s.mu.Unlock()
	if gate != nil {
		gate.once.Do(func() { gate.ctx = ctx; close(gate.entered) })
		select {
		case <-gate.resume:
		case <-ctx.Done():
			return "", false, fmt.Errorf("adoption lookup: %w", ctx.Err())
		}
	}
	s.fakeDemandSessions.mu.Lock()
	found = found && s.live[id] == label
	s.fakeDemandSessions.mu.Unlock()
	return id, found, err
}

func (s *demandAdoptionSessions) setLookup(err error, id string, found bool) {
	s.mu.Lock()
	s.err, s.id, s.found = err, id, found
	s.mu.Unlock()
}

func (s *demandAdoptionSessions) blockLookup(t *testing.T) *adoptionLookupGate {
	t.Helper()
	gate := &adoptionLookupGate{entered: make(chan struct{}), resume: make(chan struct{})}
	t.Cleanup(gate.unblock)
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()
	return gate
}

func newAdoptionManager(t *testing.T, sessions *demandAdoptionSessions) *demandManager {
	t.Helper()
	return newAdoptionManagerWithParent(context.Background(), t, sessions)
}

func newAdoptionManagerWithParent(parent context.Context, t *testing.T, sessions *demandAdoptionSessions) *demandManager {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	manager := newDemandManager(ctx, sessions, func(error) {})
	manager.adoptionRetry = 0
	manager.bind = func(uint16) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	manager.dial = func(context.Context, string) error { return nil }
	t.Cleanup(func() {
		manager.Close()
		cancel()
		joinAdoptionTransitions(t, manager)
	})
	return manager
}

func joinAdoptionTransitions(t *testing.T, manager *demandManager) {
	t.Helper()
	manager.mu.Lock()
	routes := make([]*demandRoute, 0, len(manager.routes)+len(manager.retiring))
	for _, route := range manager.routes {
		routes = append(routes, route)
	}
	for _, route := range manager.retiring {
		routes = append(routes, route)
	}
	manager.mu.Unlock()
	for _, route := range routes {
		route.mu.Lock()
		transition := route.pending
		route.mu.Unlock()
		if transition != nil {
			awaitAdoptionSignal(t, transition.done)
		}
	}
}

type adoptionCall struct {
	done chan struct{}
	err  error
}

func beginAdoptionCall(t *testing.T, call func(context.Context) error) *adoptionCall {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result := &adoptionCall{done: make(chan struct{})}
	t.Cleanup(func() {
		cancel()
		awaitAdoptionSignal(t, result.done)
	})
	go func() {
		result.err = call(ctx)
		close(result.done)
	}()
	return result
}

func awaitAdoptionSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("adoption operation did not finish within five seconds")
	}
}

func (c *adoptionCall) wait(t *testing.T) error {
	t.Helper()
	awaitAdoptionSignal(t, c.done)
	return c.err
}

func enterAdoption(manager *demandManager) func(context.Context) error {
	return func(ctx context.Context) error {
		release, err := manager.Enter(ctx, "dev")
		if release != nil {
			release()
		}
		return err
	}
}

func unresolvedAdoption(t *testing.T, service meshserve.Service) (*demandAdoptionSessions, *demandManager) {
	t.Helper()
	sessions := &demandAdoptionSessions{fakeDemandSessions: newFakeDemandSessions()}
	sessions.setLookup(errAdoptionLookup, "", false)
	sessions.live["OLD1"] = service.Label()
	manager := newAdoptionManager(t, sessions)
	manager.Sync([]meshserve.Service{service})
	return sessions, manager
}

func assertAdoptionCounts(t *testing.T, sessions *demandAdoptionSessions, wantStarted, wantStopped int) {
	t.Helper()
	if started, stopped := sessions.counts(); started != wantStarted || stopped != wantStopped {
		t.Fatalf("worker counts = started %d stopped %d, want %d and %d", started, stopped, wantStarted, wantStopped)
	}
}

func TestDemandAdoptionFailureDoesNotLaunch(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	result := beginAdoptionCall(t, enterAdoption(manager))
	err := result.wait(t)
	if started, _ := sessions.counts(); started != 0 {
		t.Fatalf("started %d sessions after adoption failed, want 0", started)
	}
	if err == nil {
		t.Fatal("Enter succeeded while adoption was unresolved")
	}
	if status := manager.Status("dev"); status.State != protocol.DemandFailed || status.Failure == "" {
		t.Fatalf("status = %+v, want a visible discovery failure", status)
	}
	assertAdoptionCounts(t, sessions, 0, 0)
	if sessions.liveCount() != 1 {
		t.Fatal("discovery failure lost the old worker")
	}
}

func TestDemandAdoptionRetryAdoptsExistingWorker(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	sessions.setLookup(nil, "OLD1", true)
	result := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, "dev") })
	if err := result.wait(t); err != nil {
		t.Fatal(err)
	}
	assertAdoptionCounts(t, sessions, 0, 0)
	if status := manager.Status("dev"); status.SessionID != "OLD1" || status.State != protocol.DemandRunning || status.Failure != "" {
		t.Fatalf("status = %+v, want adopted worker running", status)
	}
	route := manager.route("dev")
	route.mu.Lock()
	armed := route.idleTimer != nil && !route.adoptIdlePending
	route.mu.Unlock()
	if !armed {
		t.Fatal("adoption did not start a full idle window")
	}
}

func TestDemandAdoptionConfirmedAbsentLaunchesOnce(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	sessions.crash("OLD1", 0)
	sessions.setLookup(nil, "", false)
	gate := sessions.blockLookup(t)
	entered := make(chan struct{}, 2)
	manager.entered = func() { entered <- struct{}{} }
	first := beginAdoptionCall(t, enterAdoption(manager))
	second := beginAdoptionCall(t, enterAdoption(manager))
	awaitAdoptionSignal(t, entered)
	awaitAdoptionSignal(t, entered)
	awaitAdoptionSignal(t, gate.entered)
	assertAdoptionCounts(t, sessions, 0, 0)
	gate.unblock()
	if err := first.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := second.wait(t); err != nil {
		t.Fatal(err)
	}
	assertAdoptionCounts(t, sessions, 1, 0)
}

func TestDemandAdoptionUncertaintyLifecycle(t *testing.T) {
	t.Run("removed route stops discovered worker", func(t *testing.T) {
		sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
		manager.Sync(nil)
		manager.supervise()
		joinAdoptionTransitions(t, manager)
		if len(manager.Retiring()) != 1 {
			t.Fatal("removal forgot unresolved ownership")
		}
		sessions.setLookup(nil, "OLD1", true)
		manager.supervise()
		joinAdoptionTransitions(t, manager)
		manager.mu.Lock()
		retiring := manager.retiring["dev"]
		manager.mu.Unlock()
		assertAdoptionCleanupDue(t, retiring)
		assertAdoptionCounts(t, sessions, 0, 0)
		manager.supervise()
		waitForStops(t, sessions.fakeDemandSessions, 1)
		joinAdoptionTransitions(t, manager)
		manager.supervise()
		if len(manager.Retiring()) != 0 {
			t.Fatal("stopped retirement remained pending")
		}
		assertAdoptionCounts(t, sessions, 0, 1)
	})

	t.Run("label change resolves old label first", func(t *testing.T) {
		service := demandService(time.Minute)
		service.Name, service.Target = "5173", "5173"
		sessions, manager := unresolvedAdoption(t, service)
		changed := service
		changed.LocalOnly = true
		manager.Sync([]meshserve.Service{changed})
		failed := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, service.Name) })
		if err := failed.wait(t); err == nil {
			t.Fatal("changed label bypassed failed discovery")
		}
		assertAdoptionCounts(t, sessions, 0, 0)
		sessions.setLookup(nil, "OLD1", true)
		result := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, service.Name) })
		if err := result.wait(t); err != nil {
			t.Fatal(err)
		}
		assertAdoptionCounts(t, sessions, 1, 1)
		sessions.mu.Lock()
		labels := slices.Clone(sessions.labels)
		sessions.mu.Unlock()
		if !slices.Equal(labels, []string{service.Label(), service.Label(), service.Label()}) {
			t.Fatalf("looked up %v, want the original label throughout discovery", labels)
		}
		sessions.fakeDemandSessions.mu.Lock()
		replacement := sessions.live[manager.Status(service.Name).SessionID]
		sessions.fakeDemandSessions.mu.Unlock()
		if replacement != changed.Label() {
			t.Fatalf("replacement label = %q, want %q", replacement, changed.Label())
		}
	})

	t.Run("explicit stop resolves uncertainty", func(t *testing.T) {
		sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
		failed := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Stop(ctx, "dev") })
		if err := failed.wait(t); err == nil {
			t.Fatal("Stop claimed success while discovery still failed")
		}
		assertAdoptionCounts(t, sessions, 0, 0)
		sessions.setLookup(nil, "OLD1", true)
		result := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Stop(ctx, "dev") })
		if err := result.wait(t); err != nil {
			t.Fatal(err)
		}
		assertAdoptionCounts(t, sessions, 0, 1)
	})
}

func TestDemandAdoptionStaleCompletions(t *testing.T) {
	for _, found := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "live"}[found], func(t *testing.T) {
			testAdoptionCompletionAfterRemoval(t, found)
		})
	}
}

func testAdoptionCompletionAfterRemoval(t *testing.T, found bool) {
	t.Helper()
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	if !found {
		sessions.crash("OLD1", 0)
	}
	sessions.setLookup(nil, "OLD1", found)
	gate := sessions.blockLookup(t)
	result := beginAdoptionCall(t, enterAdoption(manager))
	awaitAdoptionSignal(t, gate.entered)
	manager.Sync(nil)
	gate.unblock()
	if err := result.wait(t); err == nil {
		t.Fatal("removed route admitted its stale waiter")
	}
	joinAdoptionTransitions(t, manager)
	manager.supervise()
	joinAdoptionTransitions(t, manager)
	wantStopped := 0
	if found {
		wantStopped = 1
	}
	assertAdoptionCounts(t, sessions, 0, wantStopped)
}

func TestDemandAdoptionClosePreservesWorkers(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "retiring"}[removed], func(t *testing.T) {
			testAdoptionCompletionAfterClose(t, removed)
		})
	}
}

func testAdoptionCompletionAfterClose(t *testing.T, removed bool) {
	t.Helper()
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	sessions.setLookup(nil, "OLD1", true)
	gate := sessions.blockLookup(t)
	result := beginAdoptionCall(t, enterAdoption(manager))
	awaitAdoptionSignal(t, gate.entered)
	if removed {
		manager.Sync(nil)
	}
	manager.Close()
	gate.unblock()
	if err := result.wait(t); err == nil {
		t.Fatal("closed manager admitted its stale waiter")
	}
	joinAdoptionTransitions(t, manager)
	assertAdoptionCounts(t, sessions, 0, 0)
	if sessions.liveCount() != 1 {
		t.Fatal("close lost the retained worker")
	}
}

func TestDemandAdoptionRetryIsBounded(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	route := manager.route("dev")
	route.mu.Lock()
	route.adoptRetryAt = time.Now().Add(time.Hour)
	route.mu.Unlock()
	for range 3 {
		manager.supervise()
		result := beginAdoptionCall(t, enterAdoption(manager))
		if err := result.wait(t); err == nil {
			t.Fatal("unresolved route admitted a request during backoff")
		}
	}
	sessions.mu.Lock()
	calls := len(sessions.labels)
	sessions.mu.Unlock()
	if calls != 1 {
		t.Fatalf("discovery ran %d times during backoff, want only initial adoption", calls)
	}
	assertAdoptionCounts(t, sessions, 0, 0)
}

func TestDemandAdoptionCancelledWaiterKeepsDiscovery(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	sessions.setLookup(nil, "OLD1", true)
	gate := sessions.blockLookup(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := beginAdoptionCall(t, func(context.Context) error { return manager.Start(ctx, "dev") })
	awaitAdoptionSignal(t, gate.entered)
	cancel()
	if err := result.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want cancellation", err)
	}
	gate.unblock()
	joinAdoptionTransitions(t, manager)
	assertAdoptionCounts(t, sessions, 0, 0)
	if status := manager.Status("dev"); status.SessionID != "OLD1" || status.State != protocol.DemandRunning {
		t.Fatalf("status = %+v, want adoption after the waiter leaves", status)
	}
}

func TestDemandAdoptionCancelledStopKeepsCleanup(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	sessions.setLookup(nil, "OLD1", true)
	gate := sessions.blockLookup(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := beginAdoptionCall(t, func(context.Context) error { return manager.Stop(ctx, "dev") })
	awaitAdoptionSignal(t, gate.entered)
	cancel()
	if err := result.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop = %v, want caller cancellation", err)
	}
	if err := gate.ctx.Err(); err != nil {
		t.Fatalf("caller cancellation abandoned daemon adoption: %v", err)
	}
	gate.unblock()
	joinAdoptionTransitions(t, manager)
	assertAdoptionCleanupDue(t, manager.route("dev"))
	manager.supervise()
	joinAdoptionTransitions(t, manager)
	assertAdoptionCounts(t, sessions, 0, 1)
	if sessions.liveCount() != 0 {
		t.Fatal("cancelled Stop waiter abandoned the adopted worker's cleanup")
	}
}

func TestDemandAdoptionDaemonCancellationEndsLookup(t *testing.T) {
	service := demandService(time.Minute)
	sessions := &demandAdoptionSessions{fakeDemandSessions: newFakeDemandSessions()}
	sessions.setLookup(errAdoptionLookup, "", false)
	sessions.live["OLD1"] = service.Label()
	daemon, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager := newAdoptionManagerWithParent(daemon, t, sessions)
	manager.Sync([]meshserve.Service{service})
	sessions.setLookup(nil, "OLD1", true)
	gate := sessions.blockLookup(t)
	result := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Stop(ctx, "dev") })
	awaitAdoptionSignal(t, gate.entered)
	cancel()
	awaitAdoptionSignal(t, gate.ctx.Done())
	if err := result.wait(t); err == nil {
		t.Fatal("Stop claimed success after daemon-owned discovery was cancelled")
	}
	joinAdoptionTransitions(t, manager)
	manager.Close()
	assertAdoptionCounts(t, sessions, 0, 0)
	if sessions.liveCount() != 1 {
		t.Fatal("daemon cancellation killed the retained worker")
	}
}

func TestDemandAdoptionCloseCancelsPendingStop(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	sessions.setLookup(nil, "OLD1", true)
	gate := sessions.blockLookup(t)
	result := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Stop(ctx, "dev") })
	awaitAdoptionSignal(t, gate.entered)
	manager.Close()
	gate.unblock()
	if err := result.wait(t); err == nil {
		t.Fatal("Stop claimed success after shutdown abandoned discovery")
	}
	joinAdoptionTransitions(t, manager)
	assertAdoptionCleanupDue(t, manager.route("dev"))
	manager.supervise()
	assertAdoptionCounts(t, sessions, 0, 0)
	if sessions.liveCount() != 1 {
		t.Fatal("close killed the worker behind a pending Stop")
	}
}

func assertAdoptionCleanupDue(t *testing.T, route *demandRoute) {
	t.Helper()
	if route == nil {
		t.Fatal("route forgot its retained worker")
	}
	route.mu.Lock()
	defer route.mu.Unlock()
	if route.owned != ownsUncertain || route.sessionID != "OLD1" || route.adoptLabel != "serve /dev" {
		t.Fatalf("cleanup ownership = %v, session %q, label %q", route.owned, route.sessionID, route.adoptLabel)
	}
	if route.pending != nil || !route.cleanupDueLocked() || route.adoptIdlePending || route.idleTimer != nil {
		t.Fatal("discovery must leave cleanup immediately due, without starting a stop or idle window")
	}
}

func TestDemandAdoptionSupervisorArmsIdleOnce(t *testing.T) {
	sessions, manager := unresolvedAdoption(t, demandService(time.Minute))
	sessions.setLookup(nil, "OLD1", true)
	manager.supervise()
	joinAdoptionTransitions(t, manager)
	route := manager.route("dev")
	route.mu.Lock()
	pending := route.adoptIdlePending && route.idleTimer == nil && route.owned == ownsLive
	route.mu.Unlock()
	if !pending {
		t.Fatal("retry discovery did not leave an explicit idle-arm intent")
	}
	manager.supervise()
	route.mu.Lock()
	timer, token := route.idleTimer, route.idleToken
	armed := !route.adoptIdlePending && timer != nil
	route.mu.Unlock()
	if !armed {
		t.Fatal("supervisor did not consume the pending full idle window")
	}
	manager.supervise()
	first := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, "dev") })
	second := beginAdoptionCall(t, func(ctx context.Context) error { return manager.Start(ctx, "dev") })
	if err := first.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := second.wait(t); err != nil {
		t.Fatal(err)
	}
	route.mu.Lock()
	reset := route.idleTimer != timer || route.idleToken != token || route.adoptIdlePending
	route.mu.Unlock()
	if reset {
		t.Fatal("supervisor or concurrent admission reset an already-armed idle window")
	}
	assertAdoptionCounts(t, sessions, 0, 0)
}
