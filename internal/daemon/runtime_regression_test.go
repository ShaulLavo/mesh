package daemon

import (
	"context"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"testing"
	"time"
)

func TestRuntimeRegressionRetryCannotAdoptUnknownRecipe(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sessions.adoptFor, f.sessions.adoptID = f.service.Label(), "ABCD"
	f.sync(t)
	if f.manager.Status("dev").State == protocol.DemandRunning {
		t.Fatal("initial discovery adopted unknown recipe")
	}
	f.manager.supervise()
	settleDemand(t, f.manager)
	if f.manager.Status("dev").State == protocol.DemandRunning {
		t.Fatal("retry adopted candidate without durable recipe or reference")
	}
}

func TestRuntimeRegressionRetryCannotAdoptReusedInstance(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	f.sessions.refsMu.Lock()
	replacement, _ := session.NewReservation("S001", "boot-a", session.Managed)
	f.sessions.refs["S001"] = replacement
	f.sessions.refsMu.Unlock()
	f.sessions.adoptFor, f.sessions.adoptID = f.service.Label(), "S001"
	f.reopen(t)
	if f.manager.Status("dev").State == protocol.DemandRunning {
		t.Fatal("initial restore adopted reused instance")
	}
	f.manager.supervise()
	settleDemand(t, f.manager)
	if f.manager.Status("dev").State == protocol.DemandRunning {
		t.Fatal("retry adopted reused instance despite different durable reference")
	}
}

func TestRuntimeRegressionRetryMustResumeOriginalReadiness(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	f.manager.Close()
	before := f.record(t)
	next := cloneRuntime(before)
	next.Version++
	next.Phase = storage.ServiceReadiness
	if err := f.store.CompareServiceRuntime(t.Context(), &before, &next); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Second)
	f.sessions.refsMu.Lock()
	f.sessions.unknown["S001"] = true
	f.sessions.refsMu.Unlock()
	f.reopen(t)
	f.sessions.refsMu.Lock()
	f.sessions.unknown["S001"] = false
	f.sessions.refsMu.Unlock()
	f.sessions.adoptFor, f.sessions.adoptID = f.service.Label(), "S001"
	f.manager.supervise()
	settleDemand(t, f.manager)
	if f.manager.Status("dev").State == protocol.DemandRunning {
		t.Fatal("retry bypassed expired durable readiness deadline")
	}
}

func TestRuntimeRegressionCatalogReadRecoveryUnfencesRuntime(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	if _, err := f.store.UpsertService(t.Context(), f.service); err != nil {
		t.Fatal(err)
	}
	registry, err := meshserve.NewRegistryWithReservedPrefix([]meshserve.Service{f.service}, "/mesh")
	if err != nil {
		t.Fatal(err)
	}
	faulty := &ambiguousUpsertStore{serviceStore: f.store, listFailures: 1}
	c, err := newServiceController(t.Context(), t.TempDir(), faulty, registry)
	if err != nil {
		t.Fatal(err)
	}
	c.demand = f.manager
	_, _, err = c.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeServiceLabel, RequestID: "failed-label", ServiceName: "dev", ServiceDisplayName: "New label"})
	if err == nil || !c.catalogUnknown {
		t.Fatalf("setup did not produce unknown catalog: %v", err)
	}
	if _, err := c.catalog(t.Context(), protocol.Control{Type: protocol.TypeServiceList, RequestID: "recover"}); err != nil {
		t.Fatal(err)
	}
	if c.catalogUnknown {
		t.Fatal("catalog did not recover")
	}
	if err := f.manager.Stop(t.Context(), "dev"); err != nil {
		t.Fatalf("durable catalog recovered but runtime stayed fenced: %v", err)
	}
}

func TestRuntimeRegressionStaleRouteCannotStopReaddedReplacement(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sync(t)
	old := f.manager.route("dev")
	gate := newDemandBarrier(t)
	f.sessions.mu.Lock()
	f.sessions.stopGate = gate
	f.sessions.mu.Unlock()
	f.manager.Sync(nil)
	awaitDemandSignal(t, gate.entered, "old attempt stop")
	f.service.Demand.Env = []string{"MODE=replacement"}
	f.manager.Sync([]meshserve.Service{f.service})
	gate.unblock()
	settleDemand(t, f.manager)
	if f.record(t).Attempt.Reference.ID != "S002" {
		t.Fatal("replacement not ready")
	}
	if err := old.stop(t.Context()); err == nil {
		t.Fatal("old route handle accepted a Stop and killed the readded replacement")
	}
}

func TestRuntimeRegressionReadinessFailureCleansImmediately(t *testing.T) {
	f := newRuntimeFixture(t)
	f.manager.cleanupRetry = demandCleanupRetry
	f.manager.dial = func(context.Context, string) error { return errRuntimeWrite }
	f.sync(t)
	_, stops := f.sessions.counts()
	if stops == 0 {
		t.Fatal("readiness expired but first cleanup waits for the failed-stop retry interval")
	}
	record := f.record(t)
	if record.Phase != storage.ServiceNone || record.Attempt != nil {
		t.Fatalf("successful immediate cleanup retained an attempt: %+v", record)
	}
}
