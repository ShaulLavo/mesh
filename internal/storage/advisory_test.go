package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/shaul/mesh/db/migrations"
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

// countingMigrationFiles counts migration sources opened. Goose lists sources
// by name when it builds a provider and opens one only to parse it.
type countingMigrationFiles struct {
	fs.FS
	parsed atomic.Int32
}

func (f *countingMigrationFiles) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".sql") {
		f.parsed.Add(1)
	}
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open migration source %s: %w", name, err)
	}
	return file, nil
}

func TestAdvisoryMigrationBesideHeldWriterSkipsPendingParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	writer, err := sql.Open(sqliteDriver, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := ensureAdvisoryVersionTable(t.Context(), writer); err != nil {
		t.Fatal(err)
	}
	holder, err := writer.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.ExecContext(t.Context(), "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	dsn, err := sqliteDSN(path, "0")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sources := &countingMigrationFiles{FS: migrations.Files}

	// Parsing allocates large scan buffers per line, so pending migrations the
	// opener cannot write must not be parsed at all.
	err = migrate(t.Context(), db, sources, true)
	var busy *sqlite.Error
	if !errors.As(err, &busy) || busy.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("advisory migration beside a held writer = %v, want SQLITE_BUSY", err)
	}
	if parsed := sources.parsed.Load(); parsed != 0 {
		t.Fatalf("advisory opener parsed %d migration sources it could not apply", parsed)
	}

	if _, err := holder.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(t.Context(), db, sources, true); err != nil {
		t.Fatalf("advisory migration after the writer left: %v", err)
	}
	if sources.parsed.Load() == 0 {
		t.Fatal("advisory opener applied migrations without parsing them")
	}
	assertCurrentMigrationVersion(t, &Store{db: db})
}

func TestOpenBusyTimeoutAppliesToEveryPoolConnection(t *testing.T) {
	for _, test := range []struct {
		name string
		open func(context.Context, string) (*Store, error)
		want int
	}{
		{name: "authoritative", open: Open, want: 5000},
		{name: "advisory", open: OpenAdvisory, want: 150},
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
