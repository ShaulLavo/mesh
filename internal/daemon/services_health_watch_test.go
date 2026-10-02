package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestWatchServiceHealthMatchesRealRoutesAndChanges(t *testing.T) {
	_, registry, controller := newServiceControllerTest(t, "/control")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ready")) }))
	defer upstream.Close()
	_, healthyPort, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, failedPort, err := net.SplitHostPort(refused.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err = refused.Close(); err != nil {
		t.Fatal(err)
	}
	registered := []meshserve.Service{{Name: "healthy", Kind: meshserve.Proxy, Target: healthyPort}, {Name: "failed", Kind: meshserve.Proxy, Target: failedPort}}
	if err = registry.Replace(registered); err != nil {
		t.Fatal(err)
	}
	assertServiceResponse(t, registry, "/healthy/", http.StatusOK, "ready")
	broker := newStateBroker(2, time.Now)
	controller.onCommitted = broker.servicesCommitted
	controller.publishCommitted()
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, controller, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.state = broker
	ctx, cancel := context.WithTimeout(t.Context(), runtimeTestTimeout)
	defer cancel()
	cfg := ListenerConfig{StateDir: compactSocketTempDir(t), UnixConnectionLimit: 2}
	done := runRuntime(t, ctx, cfg, server.Handle)
	conn := dialUnixRuntime(t, filepath.Join(cfg.StateDir, daemonSocketName))
	defer func() { _ = conn.Close() }()
	listed := watchContractRequest(t, conn, protocol.Control{Type: protocol.TypeServiceList, RequestID: "list"})
	watched := watchContractRequest(t, conn, protocol.Control{Type: protocol.TypeStateWatch, RequestID: "watch", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicServices}}})
	if watched.StateSnapshot == nil || !reflect.DeepEqual(watched.StateSnapshot.Services, listed.Services) {
		t.Fatalf("watch health differs from actual no-wake route probes: watch=%+v list=%+v", watched.StateSnapshot, listed.Services)
	}
	if len(listed.Services) != 2 || listed.Services[0].Healthy || listed.Services[0].Problem == "" || !listed.Services[1].Healthy {
		t.Fatalf("real route fixture did not establish healthy and failed probes: %+v", listed.Services)
	}
	upstream.Close()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	controller.observeRegistry(ctx, broker.observeServices)
	frame, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if changed.StateEvent == nil || changed.StateEvent.Payload.Service == nil || changed.StateEvent.Payload.Service.Name != "healthy" || changed.StateEvent.Payload.Service.Healthy || changed.StateEvent.Payload.Service.Problem == "" {
		t.Fatalf("actual upstream failure did not reach watch with its reason: %+v", changed)
	}
	cancel()
	if err = waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestWatchHealthSharesAdmissionAndRejectsObsoleteDefinitions(t *testing.T) {
	_, registry, controller := newServiceControllerTest(t, "/control")
	service := meshserve.Service{Name: "files", Kind: meshserve.Static, Target: t.TempDir()}
	if err := registry.Replace([]meshserve.Service{service}); err != nil {
		t.Fatal(err)
	}
	rows := map[string]protocol.ServiceInfo{service.Name: serviceDefinitionInfo(service)}
	row := rows[service.Name]
	row.Healthy = true
	rows[service.Name] = row
	controller.observedHealth, controller.healthAt = rows, time.Now()
	shared, err := controller.healthCatalog(t.Context())
	if err != nil || shared != nil {
		t.Fatalf("fresh observation was not shared: %v %+v", err, shared)
	}
	controller.healthGate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	if err := controller.observeHealth(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("health admission exceeded caller deadline: %v", err)
	}
	if len(controller.gate) != 0 {
		t.Fatal("health admission held mutation gate")
	}
	<-controller.healthGate
	changed := service
	changed.Target = t.TempDir()
	if controller.healthMatches(rows, []meshserve.Service{changed}) {
		t.Fatal("obsolete observed definition matched new route")
	}
	if next := controller.publishedServiceHealth(changed); !next.HealthUnknown || next.Healthy || next.Problem != "" {
		t.Fatalf("old probe reached replacement route: %+v", next)
	}
	controller.healthAt = time.Now().Add(-31 * time.Second)
	if next := controller.publishedServiceHealth(service); !next.HealthUnknown || next.Healthy {
		t.Fatalf("expired probe reported ready: %+v", next)
	}
}

func TestWatchHealthObservesWaitingDemandWithoutStartingIt(t *testing.T) {
	_, registry, controller := newServiceControllerTest(t, "/control")
	sessions := newFakeDemandSessions()
	manager := testDemandManager(t, sessions, func() bool { return false })
	controller.demand = manager
	service := demandService(time.Minute)
	if err := registry.Replace([]meshserve.Service{service}); err != nil {
		t.Fatal(err)
	}
	manager.Sync([]meshserve.Service{service})
	if err := controller.observeHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	row := controller.publishedServiceHealth(service)
	if !row.Healthy || row.HealthUnknown || row.Problem != "" || row.Demand == nil || row.Demand.State != protocol.DemandStopped {
		t.Fatalf("healthy waiting demand became failed/ready: %+v", row)
	}
	if started, stopped := sessions.counts(); started != 0 || stopped != 0 {
		t.Fatalf("health observation ran demand: starts=%d stops=%d", started, stopped)
	}
	// A later demand state invalidates its old probe even when its definition is unchanged.
	manager.mu.Lock()
	route := manager.routes[service.Name]
	manager.mu.Unlock()
	route.mu.Lock()
	route.state = protocol.DemandStarting
	route.mu.Unlock()
	if next := controller.publishedServiceHealth(service); !next.HealthUnknown {
		t.Fatalf("old stopped health survived changed demand: %+v", next)
	}
	if err := controller.observeHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	starting := controller.publishedServiceHealth(service)
	if !starting.Healthy || starting.HealthUnknown || starting.Problem != "" || starting.Demand.State != protocol.DemandStarting {
		t.Fatalf("starting demand did not remain waiting: %+v", starting)
	}
	if started, stopped := sessions.counts(); started != 0 || stopped != 0 {
		t.Fatalf("observing starting demand ran a process: %d/%d", started, stopped)
	}
}
