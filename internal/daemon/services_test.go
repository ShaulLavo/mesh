package daemon

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
	service := protocol.ServiceInfo{Name: "site", Kind: "static", Target: root, PublicName: "shaulavo.dev"}

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
	if err != nil || persisted.Target != root || persisted.PublicName != "shaulavo.dev" {
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
	controller, err := newServiceController(store, registry)
	if err != nil {
		t.Fatal(err)
	}
	return store, registry, controller
}

func assertServiceResponse(t *testing.T, handler http.Handler, target string, wantStatus int, wantBody string) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != wantStatus || response.Body.String() != wantBody {
		t.Fatalf("GET %s = %d %q, want %d %q", target, response.Code, response.Body.String(), wantStatus, wantBody)
	}
}
