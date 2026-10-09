package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
)

func TestServiceControllerMutatesDurableAndLiveRegistry(t *testing.T) {
	store, registry, controller := newServiceControllerTest(t, "/control")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := protocol.ServiceInfo{DisplayName: "Site", Name: "site", Kind: "static", Target: root}

	response, handled, err := controller.HandleControl(context.Background(), protocol.Control{
		Type:      protocol.TypeServiceUpsert,
		RequestID: "upsert-1",
		Service:   &service,
	})
	if err != nil || !handled {
		t.Fatalf("upsert handled = %v, error = %v", handled, err)
	}
	if response.Type != protocol.TypeServiceUpserted || response.RequestID != "upsert-1" || response.Service == nil || response.Service.Name != "site" || !response.Service.Healthy {
		t.Fatalf("upsert response = %#v", response)
	}
	assertServiceResponse(t, registry, "/site/", http.StatusOK, "live")
	persisted, err := store.GetService(context.Background(), "site")
	if err != nil || persisted.Target != canonicalServiceRoot(t, root) {
		t.Fatalf("persisted service = %#v, %v", persisted, err)
	}

	response, handled, err = controller.HandleControl(context.Background(), protocol.Control{
		Type:      protocol.TypeServiceList,
		RequestID: "list-1",
	})
	if err != nil || !handled {
		t.Fatalf("list handled = %v, error = %v", handled, err)
	}
	if response.Type != protocol.TypeServiceListed || len(response.Services) != 1 || !response.Services[0].Healthy {
		t.Fatalf("list response = %#v", response)
	}

	updatedRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(updatedRoot, "index.html"), []byte("updated"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated := protocol.ServiceInfo{Name: "site", Kind: "static", Target: updatedRoot}
	response, handled, err = controller.HandleControl(context.Background(), protocol.Control{
		Type:      protocol.TypeServiceUpsert,
		RequestID: "upsert-2",
		Service:   &updated,
	})
	if err != nil || !handled {
		t.Fatalf("update handled = %v, error = %v", handled, err)
	}
	assertServiceResponse(t, registry, "/site/", http.StatusOK, "updated")
	if response.Service.DisplayName != "Site" {
		t.Fatalf("republication lost display name: %+v", response.Service)
	}

	if err := os.Rename(updatedRoot, updatedRoot+"-gone"); err != nil {
		t.Fatal(err)
	}
	response, _, err = controller.HandleControl(context.Background(), protocol.Control{
		Type:      protocol.TypeServiceList,
		RequestID: "list-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Services) != 1 || response.Services[0].Healthy || response.Services[0].Problem == "" {
		t.Fatalf("missing-root list response = %#v", response)
	}
	assertServiceResponse(t, registry, "/site/", http.StatusServiceUnavailable, "service unavailable\n")

	for _, requestID := range []string{"delete-1", "delete-retry"} {
		response, handled, err = controller.HandleControl(context.Background(), protocol.Control{
			Type:        protocol.TypeServiceDelete,
			RequestID:   requestID,
			ServiceName: "site",
		})
		if err != nil || !handled {
			t.Fatalf("delete handled = %v, error = %v", handled, err)
		}
		if response.Type != protocol.TypeServiceDeleted || response.RequestID != requestID || response.ServiceName != "site" {
			t.Fatalf("delete response = %#v", response)
		}
	}
	assertServiceResponse(t, registry, "/site/", http.StatusNotFound, "404 page not found\n")
	if _, err := store.GetService(context.Background(), "site"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted service error = %v, want sql.ErrNoRows", err)
	}
}

func TestServiceControllerRejectsConfiguredProtocolRouteBeforeWriting(t *testing.T) {
	store, registry, controller := newServiceControllerTest(t, "/control/ws")
	root := t.TempDir()
	for _, name := range []string{"control", "control/ws", "control/ws/debug"} {
		service := protocol.ServiceInfo{Name: name, Kind: "static", Target: root}
		if _, handled, err := controller.HandleControl(context.Background(), protocol.Control{
			Type:      protocol.TypeServiceUpsert,
			RequestID: "invalid-" + name,
			Service:   &service,
		}); !handled || err == nil {
			t.Fatalf("reserved service %q handled = %v, error = %v", name, handled, err)
		}
	}
	services, err := store.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 0 || len(registry.Services()) != 0 {
		t.Fatalf("rejected services reached store %#v or registry %#v", services, registry.Services())
	}
}

func TestServiceControllerRefusesResolvedTargetChangedAfterPreview(t *testing.T) {
	home := t.TempDir()
	first := filepath.Join(home, "first")
	second := filepath.Join(home, "second")
	for _, directory := range []string{first, second} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(home, "site")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	registry, err := meshserve.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newServiceController(context.Background(), home, store, registry)
	if err != nil {
		t.Fatal(err)
	}
	requested := protocol.ServiceInfo{Name: "site", Target: "./site"}
	response, _, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeServicePreview, RequestID: "preview", Service: &requested,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	_, _, err = controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeServiceUpsert, RequestID: "upsert", Service: &requested, ServicePreview: response.ServicePreview,
	})
	if err == nil || !strings.Contains(err.Error(), "changed after preview") {
		t.Fatalf("changed target error = %v", err)
	}
	if len(registry.Services()) != 0 {
		t.Fatalf("changed target reached registry: %#v", registry.Services())
	}
}

func TestClientServerHandlesServiceRequestAndResponse(t *testing.T) {
	_, registry, controller := newServiceControllerTest(t, "/mesh")
	root := t.TempDir()
	client := newServerTestConn()
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), controller, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Handle(context.Background(), client) }()

	service := protocol.ServiceInfo{Name: "site", Kind: "files", Target: root}
	client.pushRead(serverControlFrame(t, protocol.Control{
		Type:      protocol.TypeServiceUpsert,
		RequestID: "service-1",
		Service:   &service,
	}))
	response := decodeServerControl(t, client.nextWrite(t))
	if response.Type != protocol.TypeServiceUpserted || response.Service == nil || response.Service.Name != "site" {
		t.Fatalf("service response = %#v", response)
	}
	if len(registry.Services()) != 1 {
		t.Fatalf("live registry = %#v", registry.Services())
	}

	client.pushReadError(context.Canceled)
	if err := waitServerResult(t, done, "service request server"); err != nil {
		t.Fatal(err)
	}
}

func TestServiceControllerPublishesProxyWhoseUpstreamIsDown(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	registry, err := meshserve.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newServiceController(context.Background(), t.TempDir(), store, registry)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	service := protocol.ServiceInfo{Name: "app", Kind: "proxy", Target: port}

	// Health is display-only: a down upstream must not keep the route unregistered.
	response, _, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeServiceUpsert, RequestID: "upsert", Service: &service,
	})
	want := "upstream " + address + " unreachable: connect: connection refused"
	if err != nil || response.Service == nil || response.Service.Healthy || response.Service.Problem != want {
		t.Fatalf("upsert response = %#v, error = %v, want unhealthy with %q", response, err, want)
	}

	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Skipf("port %s was taken before the upstream could reclaim it: %v", port, err)
	}
	defer listener.Close() //nolint:errcheck // test cleanup
	response, _, err = controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeServiceList, RequestID: "list",
	})
	if err != nil || len(response.Services) != 1 || !response.Services[0].Healthy || response.Services[0].Problem != "" {
		t.Fatalf("list with upstream = %#v, error = %v, want healthy", response, err)
	}
}

func newServiceControllerTest(t *testing.T, reservedPrefix string) (*storage.Store, *meshserve.Registry, *serviceController) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry, err := meshserve.NewRegistryWithReservedPrefix(nil, reservedPrefix)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newServiceController(context.Background(), t.TempDir(), store, registry)
	if err != nil {
		t.Fatal(err)
	}
	return store, registry, controller
}

type ambiguousUpsertStore struct {
	serviceStore
	mu           sync.Mutex
	upserts      int
	listFailures int
}

func (s *ambiguousUpsertStore) UpsertService(ctx context.Context, service meshserve.Service) (meshserve.Service, error) {
	s.mu.Lock()
	s.upserts++
	call := s.upserts
	s.mu.Unlock()
	persisted, err := s.serviceStore.UpsertService(ctx, service)
	if call == 1 && err == nil {
		return persisted, errors.New("injected post-commit upsert failure")
	}
	return persisted, err
}

func (s *ambiguousUpsertStore) ListServices(ctx context.Context) ([]meshserve.Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listFailures > 0 {
		s.listFailures--
		return nil, errors.New("injected durable catalog failure")
	}
	return s.serviceStore.ListServices(ctx)
}

func assertServiceResponse(t *testing.T, handler http.Handler, target string, wantStatus int, wantBody string) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != wantStatus || response.Body.String() != wantBody {
		t.Fatalf("GET %s = %d %q, want %d %q", target, response.Code, response.Body.String(), wantStatus, wantBody)
	}
}

func assertWatchCommittedService(t *testing.T, broker *stateBroker, target string) {
	t.Helper()
	broker.mu.Lock()
	defer broker.mu.Unlock()
	row, ok := broker.services["app"]
	if !ok || row.Target != target || row.Healthy || broker.observations[protocol.TopicServices].failing {
		t.Fatalf("watch differs from final routing state: %+v", row)
	}
}

func TestServiceLabelOnlyChangesDurableDisplayName(t *testing.T) {
	store, registry, controller := newServiceControllerTest(t, "/control")
	service := protocol.ServiceInfo{Name: "api", Kind: "proxy", Target: "29999", Isolate: true}
	ctx := context.Background()
	if _, _, err := controller.HandleControl(ctx, protocol.Control{Type: protocol.TypeServiceUpsert, RequestID: "publish", Service: &service}); err != nil {
		t.Fatal(err)
	}
	response, handled, err := controller.HandleControl(ctx, protocol.Control{
		Type: protocol.TypeServiceLabel, RequestID: "label", ServiceName: "api", ServiceDisplayName: "CLI Proxy",
	})
	if err != nil || !handled {
		t.Fatalf("label handled=%v error=%v", handled, err)
	}
	if response.Type != protocol.TypeServiceLabeled || response.Service == nil || response.Service.DisplayName != "CLI Proxy" {
		t.Fatalf("label response = %+v", response)
	}
	want := protocol.ServiceFromInfo(service)
	want.DisplayName = "CLI Proxy"
	persisted, err := store.GetService(ctx, "api")
	if err != nil || !persisted.Equal(want) || len(registry.Services()) != 1 || !registry.Services()[0].Equal(want) {
		t.Fatalf("label changed service definition: durable=%+v live=%+v error=%v", persisted, registry.Services(), err)
	}
	if _, _, err := controller.HandleControl(ctx, protocol.Control{Type: protocol.TypeServiceLabel, RequestID: "missing", ServiceName: "missing", ServiceDisplayName: "Missing"}); err == nil {
		t.Fatal("label created a missing route")
	}
}

func canonicalServiceRoot(t *testing.T, root string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestPrivateServiceReconcilesAmbiguousStoreCommit(t *testing.T) {
	for _, failures := range []int{0, 2} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) { verifyPrivateServiceAmbiguousCommit(t, failures) })
	}
}

func verifyPrivateServiceAmbiguousCommit(t *testing.T, failures int) {
	t.Helper()
	store, registry, _ := newServiceControllerTest(t, "/mesh")
	flaky := &ambiguousUpsertStore{serviceStore: store, listFailures: failures}
	controller, err := newServiceController(t.Context(), t.TempDir(), flaky, registry)
	if err != nil {
		t.Fatal(err)
	}
	broker := newStateBroker(DefaultSubscriberLimit, time.Now)
	controller.onCommitted = broker.servicesCommitted
	service := protocol.ServiceInfo{Name: "app", Kind: "proxy", Target: "29999"}
	_, _, err = controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeServiceUpsert, RequestID: "ambiguous", Service: &service})
	if err == nil || !strings.Contains(err.Error(), "post-commit") {
		t.Fatalf("ambiguous commit error: %v", err)
	}
	if failures > 0 && len(registry.Services()) != 0 {
		t.Fatal("unavailable durable catalog retained speculative routing")
	}
	var response protocol.Control
	for range 3 {
		response, _, err = controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeServiceList, RequestID: "recover"})
		if err == nil {
			break
		}
	}
	if err != nil || len(response.Services) != 1 || response.Services[0].Target != "29999" {
		t.Fatalf("recovered private route: %+v %v", response, err)
	}
	assertWatchCommittedService(t, broker, "29999")
}
