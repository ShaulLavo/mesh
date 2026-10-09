package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestReplaceCachedServicesIsCompleteAndDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	host := Host{ID: "host-1", MeshIdentity: "identity-1", LastSeenAt: now}
	rows := []CachedService{
		{HostID: host.ID, Service: meshserve.Service{DisplayName: "API service", Name: "api", Kind: meshserve.Proxy, Target: "3000", PrivateHost: "api.mesh.test"}, Healthy: true, ObservedAt: now},
		{HostID: host.ID, Service: meshserve.Service{Name: "blog", Kind: meshserve.Static, Target: "/srv/blog"}, Problem: "root unavailable", ObservedAt: now},
	}
	if err := store.ReplaceCachedServices(ctx, host, rows); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	got, err := store.ListCachedServices(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Service.Name != "api" || got[0].Service.DisplayName != "API service" || got[0].Service.PrivateHost != "api.mesh.test" || got[1].Service.Name != "blog" || got[1].Problem != "root unavailable" {
		t.Fatalf("cached services = %#v", got)
	}
	if err := store.ReplaceCachedServices(ctx, host, nil); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListCachedServices(ctx, host.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("cleared cache = %#v, error %v", got, err)
	}
}

func TestReplaceCachedServicesRejectsInvalidRowsBeforeChangingCache(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	now := time.Now().UTC()
	host := Host{ID: "host-1", MeshIdentity: "identity-1", LastSeenAt: now}
	valid := CachedService{HostID: host.ID, Service: meshserve.Service{DisplayName: "API service", Name: "api", Kind: meshserve.Proxy, Target: "3000"}, Healthy: true, ObservedAt: now}
	if err := store.ReplaceCachedServices(ctx, host, []CachedService{valid}); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.HostID = "another-host"
	if err := store.ReplaceCachedServices(ctx, host, []CachedService{invalid}); err == nil {
		t.Fatal("mismatched cached service host was accepted")
	}
	got, err := store.ListCachedServices(ctx, host.ID)
	if err != nil || len(got) != 1 || got[0].Service.Name != "api" {
		t.Fatalf("cache after rejected replacement = %#v, error %v", got, err)
	}
}

func TestReplaceCachedServicesRollsBackAtGlobalBound(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO hosts (id, mesh_identity, tailscale_name, last_seen_at)
		VALUES ('seed', 'seed-identity', NULL, 1);
		WITH RECURSIVE entries(value) AS (
			SELECT 1 UNION ALL SELECT value + 1 FROM entries WHERE value < 8192
		)
		INSERT INTO cached_services (
			host_id, private_name, name, kind, target, healthy, problem, observed_at
		)
		SELECT 'seed', '', printf('r%04d', value), 'proxy', '3000', 1, '', 1 FROM entries;
	`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	host := Host{ID: "host-1", MeshIdentity: "identity-1", LastSeenAt: now}
	row := CachedService{HostID: host.ID, Service: meshserve.Service{DisplayName: "API service", Name: "api", Kind: meshserve.Proxy, Target: "3000"}, Healthy: true, ObservedAt: now}
	if err := store.ReplaceCachedServices(ctx, host, []CachedService{row}); err == nil {
		t.Fatal("cache grew past its global bound")
	}
	var total int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM cached_services").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != MaximumCachedServices {
		t.Fatalf("cached row count = %d, want %d", total, MaximumCachedServices)
	}
	if _, err := store.GetHost(ctx, host.ID); err == nil {
		t.Fatal("host insert escaped the rolled-back cache transaction")
	}
}
