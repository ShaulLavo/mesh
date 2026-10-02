package daemon

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestWatchHealthRejectedBatchExpiresUnrelatedHealth(t *testing.T) {
	_, registry, controller := newServiceControllerTest(t, "/control")
	manager := testDemandManager(t, newFakeDemandSessions(), func() bool { return false })
	controller.demand = manager
	service := demandService(time.Minute)
	static := meshserve.Service{Name: "files", Kind: meshserve.Static, Target: t.TempDir()}
	registered := []meshserve.Service{service, static}
	if err := registry.Replace(registered); err != nil {
		t.Fatal(err)
	}
	broker := newStateBroker(2, time.Now)
	manager.onChange = broker.serviceDemandChanged
	controller.onCommitted = broker.servicesCommitted
	manager.Sync(registered)
	controller.publishCommitted()
	if err := controller.observeHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	oldRows := controller.observedHealth
	oldAt := time.Now().Add(-31 * time.Second)
	controller.healthAt = oldAt
	manager.mu.Lock()
	route := manager.routes[service.Name]
	manager.mu.Unlock()
	route.mu.Lock()
	route.state = protocol.DemandStarting
	route.unlock()
	err := controller.commitObservedHealth(t.Context(), oldRows)
	broker.observeServices(err)
	if err == nil || !controller.healthAt.Equal(oldAt) {
		t.Fatalf("rejected batch certified a fresh observation: err=%v age=%v", err, controller.healthAt)
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	row := broker.services[static.Name]
	if !row.HealthUnknown || row.Healthy || row.Problem != "" || !broker.observations[protocol.TopicServices].failing {
		t.Fatalf("expired unrelated service remained known after rejected batch: row=%+v observation=%+v", row, broker.observations[protocol.TopicServices])
	}
}

func TestWatchHealthStatusReadPreservesObservedDemandHealth(t *testing.T) {
	_, registry, controller := newServiceControllerTest(t, "/control")
	manager := testDemandManager(t, newFakeDemandSessions(), func() bool { return false })
	controller.demand = manager
	service := demandService(time.Minute)
	missing := meshserve.Service{Name: "missing", Kind: meshserve.Static, Target: t.TempDir() + "/missing"}
	registered := []meshserve.Service{service, missing}
	if err := registry.Replace(registered); err != nil {
		t.Fatal(err)
	}
	broker := newStateBroker(2, time.Now)
	manager.onChange = broker.serviceDemandChanged
	controller.onCommitted = broker.servicesCommitted
	manager.Sync(registered)
	controller.publishCommitted()
	if err := controller.observeHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertObserved := func() {
		t.Helper()
		broker.mu.Lock()
		defer broker.mu.Unlock()
		waiting, failed := broker.services[service.Name], broker.services[missing.Name]
		if waiting.HealthUnknown || !waiting.Healthy || waiting.Demand.State != protocol.DemandStopped || failed.HealthUnknown || failed.Healthy || failed.Problem == "" {
			t.Fatalf("status read erased actual observed health: waiting=%+v failed=%+v", waiting, failed)
		}
	}
	assertObserved()
	manager.Status(service.Name)
	assertObserved()
	observedAt := controller.healthAt
	if err := controller.observeHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !controller.healthAt.Equal(observedAt) {
		t.Fatal("matching fresh observation was probed again")
	}
	assertObserved()
	manager.mu.Lock()
	route := manager.routes[service.Name]
	manager.mu.Unlock()
	route.mu.Lock()
	route.state = protocol.DemandStarting
	route.unlock()
	broker.mu.Lock()
	defer broker.mu.Unlock()
	changed := broker.services[service.Name]
	if !changed.HealthUnknown || changed.Healthy || changed.Problem != "" || changed.Demand.State != protocol.DemandStarting {
		t.Fatalf("changed demand retained earlier health: %+v", changed)
	}
}
