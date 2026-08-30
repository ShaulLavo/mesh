package daemon

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/identity"
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
	service := protocol.ServiceInfo{Name: "site", Kind: "static", Target: root, PublicName: "site.shaulavo.dev"}

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
	if err != nil || persisted.Target != root || persisted.PublicName != "site.shaulavo.dev" {
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
	server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, controller, disabledCertificateController{})
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

func TestClientServerRoutesProoflessAndProofBearingEdgeListWhenColocated(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	serviceRegistry, err := meshserve.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &recordingServicePublisher{listed: []protocol.EdgeRouteInfo{{
		PublicName: "app.shaulavo.dev", ServiceName: "app", DisplayAlias: "Desktop", LastSeenAt: time.Now().UTC(),
	}}}
	services, err := newServiceController(context.Background(), store, serviceRegistry, publisher)
	if err != nil {
		t.Fatal(err)
	}
	edgeHost, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	originHost, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	edgeRegistry, err := edge.NewRegistry(edge.HandlerConfig{Mode: edge.ModeProxy})
	if err != nil {
		t.Fatal(err)
	}
	defer edgeRegistry.Close()
	edgeController, err := edge.NewController(context.Background(), edge.ControllerConfig{
		TargetID: edgeHost.ID,
		Origins: []edge.OriginConfig{{
			Identity: originHost.ID, DisplayAlias: "Desktop", TailscaleName: "origin.example.ts.net", ControlPort: 7337, WebSocketPath: "/mesh",
		}},
		State: store, Registry: edgeRegistry,
		Resolve: func(context.Context, edge.OriginConfig) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("100.64.0.8:7337"), nil
		},
		Pin: func(context.Context, netip.AddrPort, edge.OriginConfig) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), edgeController, services, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(context.Background(), client) }()
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeEdgeList, RequestID: "local-list", EdgeLimit: 10}))
	response := decodeServerControl(t, client.nextWrite(t))
	if response.Type != protocol.TypeEdgeListed || len(response.EdgeRoutes) != 1 {
		t.Fatalf("proofless local edge.list response = %#v", response)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{
		Type: protocol.TypeEdgeList, RequestID: "inbound-list", EdgeLimit: 10, EdgeListProof: &protocol.EdgeListProof{},
	}))
	response = decodeServerControl(t, client.nextWrite(t))
	if response.Type != protocol.TypeError {
		t.Fatalf("proof-bearing inbound edge.list response = %#v", response)
	}
	publisher.mu.Lock()
	listCalls := publisher.listCalls
	publisher.mu.Unlock()
	if listCalls != 1 {
		t.Fatalf("origin publisher list calls = %d, proof-bearing request reached service controller", listCalls)
	}
	client.pushReadError(context.Canceled)
	if err := waitServerResult(t, done, "co-located edge list server"); err != nil {
		t.Fatal(err)
	}
}

func TestPublicServiceUpsertRollsBackDurablyAndCompensatesEdge(t *testing.T) {
	for _, test := range []struct {
		name                   string
		failStoreRollback      bool
		commitThenFailRollback bool
		wantPublicName         string
	}{
		{name: "durable rollback succeeds", wantPublicName: "old.shaulavo.dev"},
		{name: "durable rollback fails", failStoreRollback: true, wantPublicName: "new.shaulavo.dev"},
		{name: "durable rollback commit is reloaded", commitThenFailRollback: true, wantPublicName: "old.shaulavo.dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close() //nolint:errcheck // test cleanup
			prior, err := base.UpsertService(context.Background(), meshserve.Service{
				Name: "app", Kind: meshserve.Proxy, Target: "8080", PublicName: "old.shaulavo.dev",
			})
			if err != nil {
				t.Fatal(err)
			}
			registry, err := meshserve.NewRegistry([]meshserve.Service{prior})
			if err != nil {
				t.Fatal(err)
			}
			store := &rollbackFailureStore{
				serviceStore: base, failRollback: test.failStoreRollback,
				commitThenFailRollback: test.commitThenFailRollback,
			}
			publisher := &recordingServicePublisher{failFirst: true}
			controller, err := newServiceController(context.Background(), store, registry, publisher)
			if err != nil {
				t.Fatal(err)
			}
			candidate := protocol.ServiceInfo{Name: "app", Kind: "proxy", Target: "8081", PublicName: "new.shaulavo.dev"}
			if _, _, err := controller.HandleControl(context.Background(), protocol.Control{
				Type: protocol.TypeServiceUpsert, RequestID: "public-update", Service: &candidate,
			}); err == nil {
				t.Fatal("unacknowledged public update succeeded")
			}
			persisted, err := base.GetService(context.Background(), "app")
			if err != nil || persisted.PublicName != test.wantPublicName || registry.Services()[0].PublicName != test.wantPublicName {
				t.Fatalf("persisted = %#v, live = %#v, error = %v", persisted, registry.Services(), err)
			}
			calls := publisher.snapshot()
			if len(calls) != 2 || calls[0][0].PublicName != "new.shaulavo.dev" || calls[1][0].PublicName != test.wantPublicName {
				t.Fatalf("edge convergence calls = %#v", calls)
			}
		})
	}
}

func TestServiceDeleteRetryRepublishesAndEdgeListIsForwardedOnlyWhenConfigured(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	registry, err := meshserve.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &recordingServicePublisher{listed: []protocol.EdgeRouteInfo{{PublicName: "app.shaulavo.dev", ServiceName: "app", DisplayAlias: "Desktop", LastSeenAt: time.Now().UTC()}}}
	controller, err := newServiceController(context.Background(), store, registry, publisher)
	if err != nil {
		t.Fatal(err)
	}
	for _, requestID := range []string{"delete", "delete-retry"} {
		if _, _, err := controller.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeServiceDelete, RequestID: requestID, ServiceName: "app"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(publisher.snapshot()) != 2 {
		t.Fatalf("delete convergence calls = %d, want 2", len(publisher.snapshot()))
	}
	response, handled, err := controller.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeEdgeList, RequestID: "edge-list", EdgeLimit: 10})
	if err != nil || !handled || len(response.EdgeRoutes) != 1 || response.Type != protocol.TypeEdgeListed {
		t.Fatalf("edge list = %#v, handled %v, error %v", response, handled, err)
	}
	disabled, err := newServiceController(context.Background(), store, registry, disabledServicePublisher{})
	if err != nil {
		t.Fatal(err)
	}
	if _, handled, err := disabled.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeEdgeList}); handled || err != nil {
		t.Fatalf("disabled edge list handled = %v, error = %v", handled, err)
	}
}

func newServiceControllerTest(t *testing.T, reservedPrefix string) (*storage.Store, *meshserve.Registry, *serviceController) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry, err := meshserve.NewRegistryWithReservedPrefix(nil, reservedPrefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newServiceController(context.Background(), store, registry, acceptingServicePublisher{})
	if err != nil {
		t.Fatal(err)
	}
	return store, registry, controller
}

type acceptingServicePublisher struct{}

func (acceptingServicePublisher) Enabled() bool { return true }
func (acceptingServicePublisher) Converge(context.Context, []meshserve.Service) error {
	return nil
}

type rollbackFailureStore struct {
	serviceStore
	mu                     sync.Mutex
	upserts                int
	failRollback           bool
	commitThenFailRollback bool
}

func (s *rollbackFailureStore) UpsertService(ctx context.Context, service meshserve.Service) (meshserve.Service, error) {
	s.mu.Lock()
	s.upserts++
	call := s.upserts
	s.mu.Unlock()
	if s.failRollback && call == 2 {
		return meshserve.Service{}, errors.New("injected durable rollback failure")
	}
	persisted, err := s.serviceStore.UpsertService(ctx, service)
	if s.commitThenFailRollback && call == 2 && err == nil {
		return persisted, errors.New("injected post-commit rollback failure")
	}
	return persisted, err
}

type recordingServicePublisher struct {
	mu        sync.Mutex
	failFirst bool
	calls     [][]meshserve.Service
	listed    []protocol.EdgeRouteInfo
	listCalls int
}

func (*recordingServicePublisher) Enabled() bool { return true }

func (p *recordingServicePublisher) Converge(_ context.Context, services []meshserve.Service) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, append([]meshserve.Service(nil), services...))
	if p.failFirst {
		p.failFirst = false
		return errors.New("injected lost edge acknowledgement")
	}
	return nil
}

func (p *recordingServicePublisher) ListPage(context.Context, string, int) ([]protocol.EdgeRouteInfo, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listCalls++
	return append([]protocol.EdgeRouteInfo(nil), p.listed...), "", nil
}

func (p *recordingServicePublisher) snapshot() [][]meshserve.Service {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([][]meshserve.Service, len(p.calls))
	for index := range p.calls {
		result[index] = append([]meshserve.Service(nil), p.calls[index]...)
	}
	return result
}
func (acceptingServicePublisher) ListPage(context.Context, string, int) ([]protocol.EdgeRouteInfo, string, error) {
	return nil, "", nil
}

func assertServiceResponse(t *testing.T, handler http.Handler, target string, wantStatus int, wantBody string) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != wantStatus || response.Body.String() != wantBody {
		t.Fatalf("GET %s = %d %q, want %d %q", target, response.Code, response.Body.String(), wantStatus, wantBody)
	}
}
