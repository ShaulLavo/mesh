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

func (c *serviceCatalogTestCache) LoadAllServices(ctx context.Context, _ []HostRecord) (map[string][]storage.CachedService, error) {
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

func (c *serviceCatalogTestCache) SaveServices(ctx context.Context, host HostRecord, _ string, rows []protocol.ServiceInfo) (bool, error) {
	if c.save != nil {
		if err := c.save(ctx); err != nil {
			return false, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saves == nil {
		c.saves = make(map[string][]protocol.ServiceInfo)
	}
	c.saves[host.ID] = append([]protocol.ServiceInfo(nil), rows...)
	return true, nil
}

func TestCollectServiceCatalogUsesCachedRowsAndMarksTimedOutHost(t *testing.T) {
	host := HostRecord{MachineName: "pc", ID: "pc-id"}
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
		}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("catalog took %s", elapsed)
	}
	if len(rows) != 1 || !rows[0].Stale || rows[0].Live || rows[0].Health() != "offline/stale" {
		t.Fatalf("cached rows = %#v", rows)
	}
	if !errors.Is(diagnostics[host.ID], context.DeadlineExceeded) {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestCollectServiceCatalogSuccessfulEmptyListClearsCacheWithoutFalseTimeout(t *testing.T) {
	host := HostRecord{MachineName: "pc", ID: "pc-id"}
	cache := &serviceCatalogTestCache{rows: map[string][]storage.CachedService{
		host.ID: {{HostID: hostID(host), Service: meshserve.Service{Name: "old", Kind: meshserve.Proxy, Target: "3000"}, ObservedAt: time.Now().UTC()}},
	}}
	rows, diagnostics, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, time.Second,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
			return remoteServiceSnapshot{}, nil
		}, cache)
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
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	token := initializeTestServicePolicy(t)
	host := HostRecord{MachineName: "pc", ID: "pc-id"}
	blocking := &serviceCatalogTestCache{load: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	lowerServiceCatalogCacheLoadCap(t, 50*time.Millisecond)
	started := time.Now()
	if _, _, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, time.Minute,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) { return remoteServiceSnapshot{}, nil }, blocking); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked cache load = %v, want its hang cap", err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("blocked cache load hung for %s", elapsed)
	}

	cache := &serviceCatalogTestCache{save: func(context.Context) error { return errors.New("disk busy") }}
	rows, diagnostics, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, time.Second,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
			return remoteServiceSnapshot{Policy: token, Services: []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000", Healthy: true}}}, nil
		}, cache)
	if err != nil || len(rows) != 1 || !rows[0].Live {
		t.Fatalf("live rows = %#v, error %v", rows, err)
	}
	var warning catalogCacheWarning
	if !errors.As(diagnostics[host.ID], &warning) || len(unavailableServiceAliases(diagnostics)) != 0 {
		t.Fatalf("cache diagnostic = %#v", diagnostics)
	}
}

// A short discovery timeout bounds remote queries only: a local cache read
// slower than it, but within the cache read bound, still succeeds.
func TestCollectServiceCatalogDiscoveryTimeoutDoesNotBoundCacheLoad(t *testing.T) {
	host := HostRecord{MachineName: "pc", ID: "pc-id"}
	timeout := 10 * time.Millisecond
	slow := &serviceCatalogTestCache{load: func(ctx context.Context) error {
		select {
		case <-time.After(5 * timeout):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	rows, diagnostics, err := CollectServiceCatalog(context.Background(), []HostRecord{host}, timeout,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) { return remoteServiceSnapshot{}, nil }, slow)
	if err != nil || len(rows) != 0 || len(diagnostics) != 0 {
		t.Fatalf("rows = %#v, diagnostics = %#v, error %v", rows, diagnostics, err)
	}
}

// lowerServiceCatalogCacheLoadCap is only for tests that are not parallel.
func lowerServiceCatalogCacheLoadCap(t *testing.T, limit time.Duration) {
	t.Helper()
	previous := serviceCatalogCacheLoadCap
	serviceCatalogCacheLoadCap = limit
	t.Cleanup(func() { serviceCatalogCacheLoadCap = previous })
}

// The agent's hard deadline covers the cache read and discovery together, yet
// reaching it still returns the cached rows rather than an error.
func TestCollectServiceCatalogByKeepsCacheReadAndDiscoveryInsideOneDeadline(t *testing.T) {
	host := HostRecord{MachineName: "pc", ID: "pc-id"}
	blocked := &serviceCatalogTestCache{load: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	if _, _, err := collectServiceCatalogBy(context.Background(), []HostRecord{host}, time.Now().Add(50*time.Millisecond),
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) { return remoteServiceSnapshot{}, nil }, blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cache read outlived the hard deadline: %v", err)
	}
	cache := &serviceCatalogTestCache{rows: map[string][]storage.CachedService{
		host.ID: {{HostID: hostID(host), Service: meshserve.Service{Name: "blog", Kind: meshserve.Proxy, Target: "3000"}, ObservedAt: time.Now().UTC()}},
	}, load: func(context.Context) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}}
	deadline := time.Now().Add(100 * time.Millisecond)
	discovery := make(chan time.Time, 1)
	rows, diagnostics, err := collectServiceCatalogBy(context.Background(), []HostRecord{host}, deadline,
		func(ctx context.Context, _ HostRecord) (remoteServiceSnapshot, error) {
			queryDeadline, _ := ctx.Deadline()
			discovery <- queryDeadline
			<-ctx.Done()
			return remoteServiceSnapshot{}, ctx.Err()
		}, cache)
	if err != nil || len(rows) != 1 || !rows[0].Stale || !errors.Is(diagnostics[host.ID], context.DeadlineExceeded) {
		t.Fatalf("rows = %#v, diagnostics = %#v, error %v", rows, diagnostics, err)
	}
	if queryDeadline := <-discovery; !queryDeadline.Equal(deadline) {
		t.Fatalf("discovery deadline %s, want the hard deadline %s", queryDeadline, deadline)
	}
}

func TestCollectServiceCatalogRejectsMergedRowsAboveGlobalBound(t *testing.T) {
	full := HostRecord{MachineName: "full", ID: "full-id"}
	extra := HostRecord{MachineName: "extra", ID: "extra-id"}
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
		}, cache)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("merged bound error = %v", err)
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
		{name: "signed name", host: HostRecord{Endpoint: "ws://100.64.0.2:7337/mesh"}, privateName: "pc.mesh.mesh.test", want: "https://pc.mesh.mesh.test/blog"},
		{name: "unconfigured website hostname", host: HostRecord{Endpoint: "ws://100.64.0.2:7337/mesh"}, privateName: "pc.other-domain.test", want: "https://pc.other-domain.test/blog"},
		{name: "malformed website hostname", host: HostRecord{Endpoint: "ws://100.64.0.2:7337/mesh"}, privateName: "pc.mesh.mesh.test/other", want: "unavailable"},
		{name: "website hostname with credentials", host: HostRecord{Endpoint: "ws://100.64.0.2:7337/mesh"}, privateName: "user@pc.mesh.mesh.test", want: "unavailable"},
		{name: "website hostname with query", host: HostRecord{Endpoint: "ws://100.64.0.2:7337/mesh"}, privateName: "pc.mesh.mesh.test?next=other", want: "unavailable"},
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
