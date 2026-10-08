package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/shaul/mesh/db/migrations"
)

type pausedMigrationHistory struct {
	database.Store
	snapshot chan struct{}
	resume   chan struct{}
	reads    atomic.Int32
}

func (s *pausedMigrationHistory) ListMigrations(ctx context.Context, db database.DBTxConn) ([]*database.ListMigrationsResult, error) {
	rows, err := s.Store.ListMigrations(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("read migration snapshot: %w", err)
	}
	// Up checks for pending work before reading the snapshot it actually applies.
	if s.reads.Add(1) != 2 {
		return rows, nil
	}
	close(s.snapshot)
	select {
	case <-s.resume:
		return rows, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for competing migration: %w", ctx.Err())
	}
}

func TestAdvisoryMigrationRaceRequiresCommittedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	dsn, err := sqliteDSN(path, "0")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureAdvisoryVersionTable(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	history, err := database.NewStore(database.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		t.Fatal(err)
	}
	paused := &pausedMigrationHistory{Store: history, snapshot: make(chan struct{}), resume: make(chan struct{})}
	loser, err := goose.NewProvider(goose.DialectCustom, db, migrations.Files, goose.WithStore(paused))
	if err != nil {
		t.Fatal(err)
	}
	winner, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan error, 1)
	finished := make(chan struct{})
	resume := sync.OnceFunc(func() { close(paused.resume) })
	t.Cleanup(func() { cancel(); resume(); <-finished })
	go func() {
		defer close(finished)
		_, err := loser.Up(ctx)
		done <- err
	}()
	select {
	case <-paused.snapshot:
	case <-ctx.Done():
		t.Fatalf("losing opener did not read its migration snapshot: %v", ctx.Err())
	}
	if _, err := winner.Up(ctx); err != nil {
		t.Fatal(err)
	}
	resume()
	var lostRace error
	select {
	case lostRace = <-done:
	case <-ctx.Done():
		t.Fatalf("losing opener did not finish: %v", ctx.Err())
	}
	var partial *goose.PartialError
	if !errors.As(lostRace, &partial) || partial.Failed.Source.Version != 1 {
		t.Fatalf("competing migration = %v, want failed version 1", lostRace)
	}
	if !isAdvisoryMigrationRace(ctx, loser, lostRace) {
		t.Fatalf("committed competing migration was classified as permanent: %v", lostRace)
	}
	version, err := loser.GetDBVersion(ctx)
	if err != nil || version != 11 {
		t.Fatalf("committed migration version = %d, %v, want 11", version, err)
	}
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'hosts'").Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("committed host tables = %d, %v, want 1", tables, err)
	}
}

func TestAdvisoryReadonlyMigrationRemainsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	dsn, err := sqliteDSN(path, "0")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureAdvisoryVersionTable(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// SQLite's read-only mode is portable and also applies when tests run as root.
	readonly, err := sql.Open(sqliteDriver, dsn+"&mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	for range 3 {
		err := migrate(t.Context(), readonly, true)
		var partial *goose.PartialError
		var sqliteErr *sqlite.Error
		if !errors.As(err, &partial) || !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_READONLY || errors.Is(err, ErrAdvisoryMigrationUnavailable) {
			t.Fatalf("read-only migration = %v, want original fatal SQLITE_READONLY", err)
		}
	}
}
