package storage

import (
	"context"
	"path/filepath"
	"testing"

	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestStoreRoundTripsIsolateAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mesh.db")
	ctx := context.Background()

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertService(ctx, meshserve.Service{Name: "app", Kind: meshserve.Proxy, Target: "3000", Isolate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertService(ctx, meshserve.Service{Name: "plain", Kind: meshserve.Proxy, Target: "3001"}); err != nil {
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
	services, err := store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 || !services[0].Isolate || services[1].Isolate {
		t.Fatalf("services after reopen = %#v, want app isolated and plain not", services)
	}

	// Turning it off must persist too, not only turning it on.
	if _, err := store.UpsertService(ctx, meshserve.Service{Name: "app", Kind: meshserve.Proxy, Target: "3000"}); err != nil {
		t.Fatal(err)
	}
	service, err := store.GetService(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if service.Isolate {
		t.Fatalf("service = %#v, want isolate cleared", service)
	}
}
