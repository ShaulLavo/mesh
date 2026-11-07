package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
)

func TestRuntimeRetryRetainsReadyOrigin(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	before := f.record(t)
	f.manager.Close()
	f.sessions.refsMu.Lock()
	f.sessions.unknown["S001"] = true
	f.sessions.refsMu.Unlock()
	f.reopen(t)
	f.clock.advance(10 * time.Second)
	f.sessions.refsMu.Lock()
	f.sessions.unknown["S001"] = false
	f.sessions.refsMu.Unlock()
	f.manager.supervise()
	settleDemand(t, f.manager)
	if status := f.manager.Status("dev"); status.State != protocol.DemandRunning || status.SessionID != "S001" {
		t.Fatalf("resolved exact instance = %+v", status)
	}
	after := f.record(t)
	if after.Version != before.Version || after.Attempt.Reference != before.Attempt.Reference || !after.Attempt.ReadyAt.Equal(before.Attempt.ReadyAt) {
		t.Fatalf("retry changed the ready attempt: before %+v, after %+v", before, after)
	}
}

func TestRuntimeRetryAppliesCurrentCleanupPhase(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	f.sessions.refsMu.Lock()
	f.sessions.unknown["S001"] = true
	f.sessions.refsMu.Unlock()
	f.reopen(t)
	gate := newDemandBarrier(t)
	f.sessions.refsMu.Lock()
	f.sessions.unknown["S001"] = false
	f.sessions.observationGate = gate
	f.sessions.refsMu.Unlock()
	f.manager.supervise()
	awaitDemandSignal(t, gate.entered, "retry exact observation")
	route := f.manager.route("dev")
	route.mu.Lock()
	adoption := route.pending
	route.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	answer := make(chan error, 1)
	go func() { answer <- f.manager.Stop(ctx, "dev") }()
	awaitRoute(t, route, "stop accepted during retry", stopAccepted)
	cleanup := f.record(t)
	if cleanup.Phase != storage.ServiceCleanup || cleanup.Attempt.StopToken == "" {
		t.Fatal("stop did not persist its exact cleanup")
	}
	route.mu.Lock()
	unchanged := route.pending == adoption
	route.mu.Unlock()
	if !unchanged {
		t.Fatal("stop replaced the in-flight adoption identity")
	}
	cancel()
	<-answer
	gate.unblock()
	settleDemand(t, f.manager)
	if starts, stops := f.sessions.counts(); starts != 1 || stops != 1 {
		t.Fatalf("retry ignored accepted cleanup: starts %d stops %d", starts, stops)
	}
	if record := f.record(t); record.Phase != storage.ServiceNone || record.Attempt != nil {
		t.Fatalf("exact cleanup not settled: %+v", record)
	}
}

func TestRuntimeReboundHandlesKeepExactStopOwner(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same recipe", true: "changed recipe"}[changed], func(t *testing.T) {
			testRuntimeReboundStop(t, changed)
		})
	}
}

func testRuntimeReboundStop(t *testing.T, changed bool) {
	t.Helper()
	f := newRuntimeFixture(t)
	f.sync(t)
	old := f.manager.route("dev")
	gate := newDemandBarrier(t)
	f.sessions.mu.Lock()
	f.sessions.stopGate = gate
	f.sessions.mu.Unlock()
	f.manager.Sync(nil)
	awaitDemandSignal(t, gate.entered, "retiring exact stop")
	before := f.record(t)
	if changed {
		f.service = cloneRuntimeDefinition(f.service)
		f.service.Demand.Env = []string{"MODE=replacement"}
	}
	f.manager.Sync([]meshserve.Service{f.service})
	current := f.manager.route("dev")
	if old == current || old.demandRouteState != current.demandRouteState {
		t.Fatal("readd did not separate definition handle from the one lifecycle owner")
	}
	after := f.record(t)
	if after.Phase != storage.ServiceCleanup || after.Attempt.Reference != before.Attempt.Reference || after.Attempt.StopToken != before.Attempt.StopToken {
		t.Fatalf("readd replaced the admitted stop obligation: before %+v, after %+v", before, after)
	}
	if err := old.stop(t.Context()); err == nil {
		t.Fatal("old definition accepted another stop")
	}
	if err := old.start(t.Context()); err == nil {
		t.Fatal("old definition accepted another start")
	}
	if starts, _ := f.sessions.counts(); starts != 1 {
		t.Fatalf("replacement overlapped old stop: %d starts", starts)
	}
	gate.unblock()
	settleDemand(t, f.manager)
	if starts, stops := f.sessions.counts(); starts != 2 || stops != 1 {
		t.Fatalf("rebound owner starts %d stops %d", starts, stops)
	}
	if err := current.stop(t.Context()); err != nil {
		t.Fatalf("current handle refused its own stop: %v", err)
	}
}

func TestRuntimeDefinitionEditInvalidatesSelectedHandle(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	old := f.manager.route("dev")
	f.service = cloneRuntimeDefinition(f.service)
	f.service.DisplayName = "New display name"
	f.sync(t)
	if err := old.stop(t.Context()); err == nil {
		t.Fatal("selected handle admitted a control after its definition changed")
	}
	if starts, stops := f.sessions.counts(); starts != 1 || stops != 0 {
		t.Fatalf("display edit or stale handle disturbed worker: starts %d stops %d", starts, stops)
	}
}
