package storage

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/shaul/mesh/db/migrations"
)

func TestPrivateAppStateMigrationPreservesBytesAndRefusesOldReaders(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mesh.db")
	historical := historicalDatabase(t, path, 11)
	legacy := []byte(`{"apps":{"7k3d":{"id":"7k3d","visibility":"public","status":"active"}},"owners":{}}`)
	if _, err := historical.ExecContext(ctx, "INSERT INTO app_state (key,data) VALUES (?,?)", "apps.edge", legacy); err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, historical, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: historical}
	data, err := store.LoadAppState(ctx, "apps.edge")
	if err != nil || !bytes.Equal(data, legacy) {
		t.Fatalf("migration changed owner state: %q %v", data, err)
	}
	var oldData []byte
	if err := store.db.QueryRowContext(ctx, "SELECT data FROM app_state WHERE key = ?", "apps.edge").Scan(&oldData); err == nil {
		t.Fatal("legacy app reader can read private-only state")
	}
	provider, err = goose.NewProvider(goose.DialectSQLite3, store.db, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 11); err == nil {
		t.Fatal("app-state downgrade reopened legacy sharing")
	}
	data, err = store.LoadAppState(ctx, "apps.edge")
	if err != nil || !bytes.Equal(data, legacy) {
		t.Fatalf("refused downgrade lost state: %q %v", data, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Close() })
	assertAppStateBytes(t, current, legacy)
}
