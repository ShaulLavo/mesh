package cli

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	"github.com/shaul/mesh/internal/storage"
)

func seedCatalogMigrationHistory(t *testing.T, stateDir string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(stateDir, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	store, err := database.NewStore(database.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateVersionTable(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(t.Context(), db, database.InsertRequest{Version: 0}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCatalogOpenersKeepFreshMigrationsOptional(t *testing.T) {
	// Repeat cold starts so Goose's stale applied-version snapshot is exercised.
	for range 100 {
		t.Setenv("MESH_STATE_DIR", t.TempDir())
		start := make(chan struct{})
		var openers sync.WaitGroup
		for range 8 {
			openers.Go(func() {
				<-start
				cache, err := OpenCatalogCache(t.Context())
				if err != nil {
					t.Errorf("concurrent cache migration blocked live queries: %v", err)
					return
				}
				defer func() {
					if err := cache.Close(); err != nil {
						t.Error(err)
					}
				}()
				if _, err := cache.LoadAllServices(t.Context()); err != nil {
					t.Errorf("opened cache has incomplete service schema: %v", err)
				}
				host := HostRecord{ID: "host-pc", MachineName: "pc"}
				_, warning := cache.LoadServices(t.Context(), host)
				if (cache.store == nil) != (warning != nil) {
					t.Errorf("optional migration warning = %v, cache available = %t", warning, cache.store != nil)
				}
				if _, err := cache.LoadServices(t.Context(), host); err != nil {
					t.Errorf("optional migration diagnostic repeated: %v", err)
				}
			})
		}
		close(start)
		openers.Wait()
		cache, err := OpenCatalogCache(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if cache.store == nil {
			t.Error("cache did not recover after concurrent migration work settled")
		}
		if err := cache.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogMigrationFailureProducesOneOptionalWarning(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("MESH_STATE_DIR", stateDir)
	seedCatalogMigrationHistory(t, stateDir)
	db, err := sql.Open("sqlite", filepath.Join(stateDir, catalogDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	// A stale migration snapshot tries to create a table another opener created.
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE hosts (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	}()
	host := HostRecord{ID: "host-pc", MachineName: "pc"}
	if rows, err := cache.LoadServices(t.Context(), host); len(rows) != 0 || !errors.Is(err, storage.ErrAdvisoryMigrationUnavailable) {
		t.Fatalf("cache warning = %#v, %v, want unavailable migration", rows, err)
	}
	if rows, err := cache.LoadServices(t.Context(), host); len(rows) != 0 || err != nil {
		t.Fatalf("migration warning repeated = %#v, %v", rows, err)
	}
	store, err := storage.Open(t.Context(), filepath.Join(stateDir, catalogDatabaseName))
	if store != nil {
		t.Cleanup(func() { _ = store.Close() })
	}
	var partial *goose.PartialError
	if !errors.As(err, &partial) || errors.Is(err, storage.ErrAdvisoryMigrationUnavailable) {
		t.Fatalf("authoritative migration failure = %v, want original fatal error", err)
	}
}
