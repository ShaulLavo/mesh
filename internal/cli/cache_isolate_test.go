package cli

import (
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

func TestCachedServicePreservesIsolationAcrossReopen(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	host := HostRecord{ID: "host-1", Alias: "pc", MeshIdentity: "identity-1"}
	for _, isolated := range []bool{true, false} {
		checkCachedServiceIsolation(t, host, isolated)
	}
}

func checkCachedServiceIsolation(t *testing.T, host HostRecord, isolated bool) {
	t.Helper()
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rows := []protocol.ServiceInfo{{Name: "app", Kind: "proxy", Target: "3000", Isolate: isolated, Healthy: true}}
	if err := cache.SaveServices(t.Context(), host, "", rows); err != nil {
		_ = cache.Close()
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close() //nolint:errcheck // test cleanup
	cached, err := cache.LoadServices(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := cachedServiceCatalogRows(host, cached)
	if len(catalog) != 1 || catalog[0].Service.Isolate != isolated {
		t.Fatalf("cached catalog = %#v, want isolation %t", catalog, isolated)
	}
	all, err := cache.LoadAllServices(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all[host.ID]) != 1 || all[host.ID][0].Service.Isolate != isolated {
		t.Fatalf("all cached services = %#v, want isolation %t", all, isolated)
	}
}
