package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
)

type serviceCatalogTestCache struct {
	mu    sync.Mutex
	rows  map[string][]storage.CachedService
	saves map[string][]protocol.ServiceInfo
	load  func(context.Context) error
	save  func(context.Context) error
}

func (c *serviceCatalogTestCache) LoadAllServices(ctx context.Context) (map[string][]storage.CachedService, error) {
	if c.load != nil {
		if err := c.load(ctx); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string][]storage.CachedService, len(c.rows))
	for key, rows := range c.rows {
		result[key] = append([]storage.CachedService(nil), rows...)
	}
	return result, nil
}

func (c *serviceCatalogTestCache) SaveServices(ctx context.Context, host HostRecord, _ string, rows []protocol.ServiceInfo) error {
	if c.save != nil {
		if err := c.save(ctx); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saves == nil {
		c.saves = make(map[string][]protocol.ServiceInfo)
	}
	c.saves[host.ID] = append([]protocol.ServiceInfo(nil), rows...)
	return nil
}

func TestCollectServiceCatalogUsesCachedRowsAndMarksTimedOutHost(t *testing.T) {
	host := HostRecord{Alias: "pc", ID: "pc-id"}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	cache := &serviceCatalogTestCache{rows: map[string][]storage.CachedService{
		host.ID: {{
			HostID: hostID(host), Service: meshserve.Service{Name: "blog", Kind: meshserve.Proxy, Target: "3000"},
			Healthy: true, ObservedAt: now,
		}},
	}}
	started := time.Now()
	rows, diagnostics, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, 20*time.Millisecond,
		func(ctx context.Context, _ HostRecord) (remoteServiceSnapshot, error) {
			<-ctx.Done()
			return remoteServiceSnapshot{}, ctx.Err()
		}, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("catalog took %s", elapsed)
	}
	if len(rows) != 1 || !rows[0].Stale || rows[0].Live || rows[0].Health() != "offline/stale" {
		t.Fatalf("cached rows = %#v", rows)
	}
	if !errors.Is(diagnostics[host.Alias], context.DeadlineExceeded) {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestCollectServiceCatalogSuccessfulEmptyListClearsCacheWithoutFalseTimeout(t *testing.T) {
	host := HostRecord{Alias: "pc", ID: "pc-id"}
	cache := &serviceCatalogTestCache{rows: map[string][]storage.CachedService{
		host.ID: {{HostID: hostID(host), Service: meshserve.Service{Name: "old", Kind: meshserve.Proxy, Target: "3000"}, ObservedAt: time.Now().UTC()}},
	}}
	rows, diagnostics, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, time.Second,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
			return remoteServiceSnapshot{}, nil
		}, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(diagnostics) != 0 {
		t.Fatalf("rows = %#v, diagnostics = %#v", rows, diagnostics)
	}
	cache.mu.Lock()
	saved, exists := cache.saves[host.ID]
	cache.mu.Unlock()
	if !exists || len(saved) != 0 {
		t.Fatalf("empty live list was not cached: %#v", cache.saves)
	}
}

func TestCollectServiceCatalogBoundsCacheLoadAndTreatsSaveFailureAsWarning(t *testing.T) {
	host := HostRecord{Alias: "pc", ID: "pc-id"}
	blocking := &serviceCatalogTestCache{load: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	started := time.Now()
	if _, _, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, 15*time.Millisecond,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) { return remoteServiceSnapshot{}, nil }, nil, blocking); err == nil {
		t.Fatal("blocked cache load succeeded")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("bounded cache load took %s", elapsed)
	}

	cache := &serviceCatalogTestCache{save: func(context.Context) error { return errors.New("disk busy") }}
	rows, diagnostics, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, time.Second,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
			return remoteServiceSnapshot{Services: []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000", Healthy: true}}}, nil
		}, nil, cache)
	if err != nil || len(rows) != 1 || !rows[0].Live {
		t.Fatalf("live rows = %#v, error %v", rows, err)
	}
	var warning catalogCacheWarning
	if !errors.As(diagnostics[host.Alias], &warning) || len(unavailableServiceAliases(diagnostics)) != 0 {
		t.Fatalf("cache diagnostic = %#v", diagnostics)
	}
}

func TestCollectServiceCatalogRejectsMergedRowsAboveGlobalBound(t *testing.T) {
	full := HostRecord{Alias: "full", ID: "full-id"}
	extra := HostRecord{Alias: "extra", ID: "extra-id"}
	rows := make([]storage.CachedService, storage.MaximumCachedServices)
	for index := range rows {
		rows[index] = storage.CachedService{
			HostID: hostID(full), Service: meshserve.Service{Name: fmt.Sprintf("r%04d", index), Kind: meshserve.Proxy, Target: "3000"},
			ObservedAt: time.Now().UTC(),
		}
	}
	cache := &serviceCatalogTestCache{rows: map[string][]storage.CachedService{full.ID: rows}}
	_, _, err := CollectServiceCatalog(context.Background(), []HostRecord{full, extra}, time.Second,
		func(_ context.Context, host HostRecord) (remoteServiceSnapshot, error) {
			if host.ID == full.ID {
				return remoteServiceSnapshot{}, errors.New("offline")
			}
			return remoteServiceSnapshot{Services: []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000", Healthy: true}}}, nil
		}, nil, cache)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("merged bound error = %v", err)
	}
}

func TestCollectServiceCatalogMarksPublicHealthUnknownWithoutEdgeStatus(t *testing.T) {
	host := HostRecord{Alias: "pc", ID: "pc-id"}
	cache := &serviceCatalogTestCache{}
	rows, diagnostics, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, time.Second,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
			return remoteServiceSnapshot{Services: []protocol.ServiceInfo{{
				Name: "blog", Kind: "proxy", Target: "3000", PublicName: "blog.shaulavo.dev", Healthy: true,
			}}}, nil
		}, func(context.Context, HostRecord) ([]protocol.EdgeRouteInfo, error) {
			return nil, errors.New("edge offline")
		}, cache)
	if err != nil || len(diagnostics) != 0 || len(rows) != 1 {
		t.Fatalf("catalog rows = %#v, diagnostics = %#v, error = %v", rows, diagnostics, err)
	}
	if rows[0].Health() != "edge-unknown" || rows[0].EdgeKnown {
		t.Fatalf("public row without edge status = %#v, health %q", rows[0], rows[0].Health())
	}
}

func TestServiceURLPrefersSignedNameAndUsesVerifiedControlEndpointFallback(t *testing.T) {
	service := protocol.ServiceInfo{Name: "blog", Kind: "proxy", Target: "3000"}
	for _, test := range []struct {
		name        string
		host        HostRecord
		privateName string
		want        string
	}{
		{name: "signed name", host: HostRecord{Endpoint: "ws://100.64.0.2:7337/mesh"}, privateName: "pc.mesh.shaulavo.dev", want: "https://pc.mesh.shaulavo.dev/blog"},
		{name: "plain IPv4 endpoint", host: HostRecord{Endpoint: "ws://100.64.0.2:7337/control/ws"}, want: "http://100.64.0.2:7337/blog"},
		{name: "TLS IPv6 endpoint", host: HostRecord{Endpoint: "wss://[fd7a:115c:a1e0::2]:7337/mesh"}, want: "https://[fd7a:115c:a1e0::2]:7337/blog"},
		{name: "unsupported endpoint", host: HostRecord{Endpoint: "http://100.64.0.2:7337/mesh"}, want: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := serviceURL(test.host, test.privateName, service); got != test.want {
				t.Fatalf("service URL = %q, want %q", got, test.want)
			}
		})
	}
}

func hostID(host HostRecord) storage.HostID { return storage.HostID(host.ID) }
