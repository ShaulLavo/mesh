package cli

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

func TestAgentServiceCacheRoundTripPreservesScope(t *testing.T) {
	for _, test := range []struct {
		name    string
		service protocol.ServiceInfo
		scope   string
	}{
		{"local listener", protocol.ServiceInfo{Name: "3000", Kind: "proxy", Target: "3000", LocalOnly: true, Listens: []protocol.ServiceListen{{Public: 3000, Upstream: 3001}}}, "local"},
		{"numeric tailnet listener", protocol.ServiceInfo{Name: "3000", Kind: "proxy", Target: "3000", Listens: []protocol.ServiceListen{{Public: 3000, Upstream: 3001}}}, "tailnet"},
		{"named tailnet proxy", protocol.ServiceInfo{Name: "api", Kind: "proxy", Target: "3000"}, "tailnet"},
	} {
		t.Run(test.name, func(t *testing.T) { checkAgentServiceCacheScope(t, test.service, test.scope) })
	}
}

func checkAgentServiceCacheScope(t *testing.T, service protocol.ServiceInfo, scope string) {
	t.Helper()
	observed := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cache := &SQLiteCatalogCache{store: store, now: func() time.Time { return observed }}
	id := argumentIdentities(t)[1]
	host := HostRecord{ID: id, MeshIdentity: id, Endpoint: "ws://127.0.0.1:1/control/ws"}
	live := liveServiceCatalogRows(host, remoteServiceSnapshot{Host: host, Services: []protocol.ServiceInfo{service}})
	if got := agentServiceRow(live[0], observed).Scope; got != scope {
		t.Fatalf("live scope %q, want %q", got, scope)
	}
	if err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{service}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	cache = &SQLiteCatalogCache{store: reopened, now: func() time.Time { return observed }}
	offline := errors.New("fixture host offline")
	rows, diagnostics, err := CollectServiceCatalog(t.Context(), []HostRecord{host}, time.Second, func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
		return remoteServiceSnapshot{}, offline
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(diagnostics[host.ID], offline) {
		t.Fatalf("offline diagnostic lost: %v", diagnostics)
	}
	if len(rows) != 1 {
		t.Fatalf("cached rows %d, want 1", len(rows))
	}
	envelope := servicesEnvelope(rows, diagnostics, []HostRecord{host}, observed.Add(time.Minute))
	data, ok := envelope.Data.(agentapi.ServicesData)
	if !ok || envelope.Status != agentapi.StatusPartial || len(data.Services) != 1 {
		t.Fatalf("missing useful cached partial result: %+v", envelope)
	}
	result := data.Services[0]
	if result.Scope != scope {
		t.Fatalf("scope changed from %q to %q after SQLite round trip", scope, result.Scope)
	}
	if result.Freshness != agentapi.FreshnessCached || result.URL != nil || result.Health != "offline/stale" || result.ObservedAt == nil || !result.ObservedAt.Equal(observed) {
		t.Fatalf("cached observations changed: %+v", result)
	}
	if rows[0].Service.LocalOnly != service.LocalOnly || !slices.Equal(rows[0].Service.Listens, service.Listens) {
		t.Fatalf("cached scope evidence lost: local=%v listeners=%v", rows[0].Service.LocalOnly, rows[0].Service.Listens)
	}
}
