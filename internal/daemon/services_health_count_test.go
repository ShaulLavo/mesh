package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

type countChurnFixture struct {
	controller *serviceController
	manager    *demandManager
	broker     *stateBroker
	sub        *stateSubscriber
	service    meshserve.Service
}

func newCountChurnFixture(t *testing.T) countChurnFixture {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fixture ready"))
	}))
	t.Cleanup(upstream.Close)
	_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, registry, controller := newServiceControllerTest(t, "/fixture-control")
	manager := testDemandManager(t, newFakeDemandSessions(), func() bool { return true })
	controller.demand = manager
	service := demandService(time.Minute)
	service.Name, service.Target, service.Listens = "fixture-proxy", port, nil
	service.Demand.Command, service.Demand.Cwd = "fixture-never-executed", t.TempDir()
	if err := registry.Replace([]meshserve.Service{service}); err != nil {
		t.Fatal(err)
	}
	broker := newStateBroker(2, time.Now)
	controller.onCommitted, manager.onChange = broker.servicesCommitted, broker.serviceDemandChanged
	manager.Sync([]meshserve.Service{service})
	if err := manager.Start(t.Context(), service.Name); err != nil {
		t.Fatal(err)
	}
	for range 49 {
		release, err := manager.Enter(t.Context(), service.Name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	controller.publishCommitted()
	if err := controller.observeHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	sub, snapshot, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicServices}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { broker.unsubscribe(sub) })
	if len(snapshot.Services) != 1 {
		t.Fatal("known-good snapshot missing service")
	}
	row := snapshot.Services[0]
	if !row.Healthy || row.HealthUnknown || row.Problem != "" || row.Demand.State != protocol.DemandRunning || row.Demand.Connections != 49 {
		t.Fatal("known-good running healthy observation not visible")
	}
	t.Logf("known-good watch healthy=%t unknown=%t state=%s connections=%d", row.Healthy, row.HealthUnknown, row.Demand.State, row.Demand.Connections)
	return countChurnFixture{controller, manager, broker, sub, service}
}

func (f countChurnFixture) countOnlyChange(t *testing.T) protocol.ServiceInfo {
	t.Helper()
	before := f.manager.Status(f.service.Name)
	release, err := f.manager.Enter(t.Context(), f.service.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	after := f.manager.Status(f.service.Name)
	before.Connections++
	if !reflect.DeepEqual(before, after) {
		t.Fatal("connection admission changed a demand field besides Connections")
	}
	for _, frame := range f.broker.take(f.sub) {
		if frame.StateEvent != nil && frame.StateEvent.Kind == "service.changed" && frame.StateEvent.Payload.Service != nil {
			row := *frame.StateEvent.Payload.Service
			t.Logf("count-only watch event healthy=%t unknown=%t state=%s connections=%d", row.Healthy, row.HealthUnknown, row.Demand.State, row.Demand.Connections)
			return row
		}
	}
	t.Fatal("count-only change did not publish a service.changed event")
	return protocol.ServiceInfo{}
}

func TestConnectionCountRetainsHealthyWatch(t *testing.T) {
	f := newCountChurnFixture(t)
	watched := f.countOnlyChange(t)
	listed, handled, err := f.controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeServiceList, RequestID: "fixture-list"})
	if err != nil || !handled || len(listed.Services) != 1 {
		t.Fatalf("direct list fixture failed: handled=%t err=%v", handled, err)
	}
	direct := listed.Services[0]
	t.Logf("direct list healthy=%t unknown=%t state=%s connections=%d", direct.Healthy, direct.HealthUnknown, direct.Demand.State, direct.Demand.Connections)
	if !direct.Healthy || direct.HealthUnknown || direct.Demand.Connections != 50 {
		t.Fatal("live direct probe did not remain healthy at the new connection count")
	}
	if watched.HealthUnknown || !watched.Healthy || watched.Demand.Connections != 50 {
		t.Fatalf("count-only demand update erased healthy watch: healthy=%t unknown=%t connections=%d; direct list healthy=%t unknown=%t connections=%d", watched.Healthy, watched.HealthUnknown, watched.Demand.Connections, direct.Healthy, direct.HealthUnknown, direct.Demand.Connections)
	}
}

func TestConnectionCountAllowsObservedHealthPublication(t *testing.T) {
	f := newCountChurnFixture(t)
	row := f.controller.serviceStatuses(t.Context(), []meshserve.Service{f.service})[0]
	if !row.Healthy || row.HealthUnknown || row.Demand.Connections != 49 {
		t.Fatal("actual pre-change health probe was not healthy")
	}
	f.countOnlyChange(t)
	err := f.controller.commitObservedHealth(t.Context(), map[string]protocol.ServiceInfo{row.Name: row})
	if err != nil {
		t.Fatalf("count-only churn rejected healthy observation: %v", err)
	}
}

func TestDemandHealthRetainsOnlyCountChanges(t *testing.T) {
	cases := []struct {
		name   string
		change func(*demandRoute)
	}{
		{"lifecycle", func(r *demandRoute) { r.state = protocol.DemandStarting }},
		{"session", func(r *demandRoute) { r.sessionID = "replacement-fixture-session" }},
		{"listener", func(r *demandRoute) { r.unbound[12345] = "fixture listener unavailable" }},
		{"failure", func(r *demandRoute) { r.state, r.failure = protocol.DemandFailed, "fixture session exited" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertDemandHealthInvalidated(t, tc.name, tc.change) })
	}
}

func assertDemandHealthInvalidated(t *testing.T, name string, change func(*demandRoute)) {
	t.Helper()
	f := newCountChurnFixture(t)
	old := f.controller.observedHealth
	route := f.manager.route(f.service.Name)
	route.mu.Lock()
	change(route)
	route.unlock()
	if f.controller.healthMatches(old, []meshserve.Service{f.service}) {
		t.Fatal("genuine demand change accepted prior observation")
	}
	row := f.controller.publishedServiceHealth(f.service)
	if !row.HealthUnknown || row.Healthy {
		t.Fatal("genuine change retained old health")
	}
	if err := f.controller.commitObservedHealth(t.Context(), old); err == nil {
		t.Fatal("genuine demand change accepted obsolete publication")
	}
	if err := f.controller.observeHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	row = f.controller.publishedServiceHealth(f.service)
	if row.HealthUnknown {
		t.Fatal("genuine changed state could not be freshly observed")
	}
	if (name == "listener" || name == "failure") && (row.Healthy || row.Problem == "") {
		t.Fatal("genuine listener/session failure was hidden")
	}
}
