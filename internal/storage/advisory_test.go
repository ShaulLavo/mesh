package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenAdvisoryAppliesPendingMigrationsWithExistingHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	db, err := sql.Open(sqliteDriver, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureAdvisoryVersionTable(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, err := OpenAdvisory(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		assertCurrentMigrationVersion(t, store)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var initialVersions int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM goose_db_version WHERE version_id = 0").Scan(&initialVersions); err != nil {
		t.Fatal(err)
	}
	if initialVersions != 1 {
		t.Fatalf("initial migration rows = %d, want 1", initialVersions)
	}
}

func TestOpenBusyTimeoutAppliesToEveryPoolConnection(t *testing.T) {
	for _, test := range []struct {
		name string
		open func(context.Context, string) (*Store, error)
		want int
	}{
		{name: "authoritative", open: Open, want: 5000},
		{name: "advisory", open: OpenAdvisory, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := test.open(t.Context(), filepath.Join(t.TempDir(), "mesh.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			assertSQLiteSettings(t, store)
			assertCurrentMigrationVersion(t, store)
			connections := make([]*sql.Conn, 0, sqliteMaxOpenConns)
			t.Cleanup(func() {
				for _, conn := range connections {
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			for index := range sqliteMaxOpenConns {
				conn, err := store.db.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				connections = append(connections, conn)
				var timeout int
				if err := conn.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&timeout); err != nil {
					t.Fatal(err)
				}
				if timeout != test.want {
					t.Fatalf("connection %d busy_timeout = %d, want %d", index, timeout, test.want)
				}
			}
		})
	}
}
