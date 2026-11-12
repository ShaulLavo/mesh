package storage

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenReadOnlyDoesNotCreateOrMigrateAndRejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	if store, err := OpenReadOnly(t.Context(), path); err == nil {
		_ = store.Close()
		t.Fatal("read-only open created a missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database was created: %v", err)
	}
	db, err := sql.Open(sqliteDriver, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE hosts(id TEXT PRIMARY KEY, mesh_identity TEXT NOT NULL, tailscale_name TEXT, last_seen_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO hosts VALUES ('peer','key',NULL,1700000000000)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host, err := store.GetHost(t.Context(), "peer")
	if err != nil || host.MeshIdentity != "key" || !host.LastSeenAt.Equal(time.UnixMilli(1700000000000)) {
		t.Fatalf("read-only metadata: %+v %v", host, err)
	}
	if _, err := store.UpsertHost(t.Context(), Host{ID: "new", MeshIdentity: "new-key", LastSeenAt: time.Now()}); err == nil {
		t.Fatal("read-only store wrote metadata")
	}
	var tables int
	if err := store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE name='goose_db_version'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("read-only open ran migrations: %d %v", tables, err)
	}
}
