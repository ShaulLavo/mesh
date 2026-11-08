package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"modernc.org/sqlite"

	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/session"
	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

func runtimeTestRecord(t *testing.T) ServiceRuntime {
	t.Helper()
	recipe, err := meshserve.Normalize(meshserve.Service{Name: "dev", Kind: meshserve.Proxy, Target: "5173", Demand: &meshserve.Demand{Command: "run", Cwd: "/tmp", Env: []string{"MODE=one"}, ReadyTimeout: time.Second, KeepRunning: true}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := session.NewReservation("ABCD", "boot", session.Managed)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return ServiceRuntime{Name: "dev", Generation: "1234567890abcdef1234567890abcdef", Version: 1, Phase: ServiceReadiness, Attempt: &ServiceAttempt{Reference: ref, Launched: recipe, StartedAt: now, Deadline: now.Add(time.Second)}}
}

func TestServiceRuntimeRoundTripAndCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeTestRecord(t)
	if err := store.CompareServiceRuntime(t.Context(), nil, &record); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	got, err := store.GetServiceRuntime(t.Context(), record.Name)
	if err != nil || !got.Equal(record) {
		t.Fatalf("roundtrip = %+v, %v", got, err)
	}
	// A retry of an accepted write still performs its synchronous barrier.
	if err := store.CompareServiceRuntime(t.Context(), nil, &record); err != nil {
		t.Fatal(err)
	}
	next := got
	next.Version++
	next.Failures = 3
	if err := store.CompareServiceRuntime(t.Context(), &got, &next); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &got, nil); !errors.Is(err, ErrServiceRuntimeConflict) {
		t.Fatalf("stale delete = %v", err)
	}
	next.Attempt, next.Phase = nil, ServiceNone
	next.Version++
	before, _ := store.GetServiceRuntime(t.Context(), record.Name)
	if err := store.CompareServiceRuntime(t.Context(), &before, &next); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertService(t.Context(), record.Attempt.Launched); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &next, nil); !errors.Is(err, ErrServiceRuntimeConflict) {
		t.Fatalf("bound definition delete = %v", err)
	}
	if err := store.DeleteService(t.Context(), record.Name); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &next, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &next, nil); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRuntimeRejectsIncompleteOwnership(t *testing.T) {
	for _, phase := range []ServicePhase{ServiceStarting, ServiceReadiness, ServiceReady, ServiceCleanup} {
		t.Run(string(phase), func(t *testing.T) { assertIncompleteRuntimePhase(t, phase) })
	}
}

func assertIncompleteRuntimePhase(t *testing.T, phase ServicePhase) {
	t.Helper()
	record := runtimeTestRecord(t)
	record.Phase, record.Attempt = phase, nil
	if err := validateServiceRuntime(record); err == nil {
		t.Fatal("phase without ownership accepted")
	}
}

func TestServiceRuntimeDeletesAfterRecipeRemoval(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	record := runtimeTestRecord(t)
	service := record.Attempt.Launched
	record.Phase, record.Attempt = ServiceNone, nil
	if err := store.CompareServiceRuntime(t.Context(), nil, &record); err != nil {
		t.Fatal(err)
	}
	service.Demand = nil
	if _, err := store.UpsertService(t.Context(), service); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &record, nil); err != nil {
		t.Fatalf("a definition without a worker recipe pinned retired ownership: %v", err)
	}
}

func TestServiceRuntimeCatalogWriterCancellation(t *testing.T) {
	for _, name := range []string{"insert", "update", "delete", "confirm-insert", "confirm-update", "confirm-delete"} {
		t.Run(name, func(t *testing.T) { runtimeCatalogWriterCancellation(t, name) })
	}
}

func runtimeCatalogWriterCancellation(t *testing.T, name string) {
	t.Helper()
	store := openTestStore(t)
	before, next := runtimeContentionTransition(t, store, name)
	revision := runtimeBarrierRevision(t, store)
	stored, err := store.ListServiceRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var previous *ServiceRuntime
	if len(stored) == 1 {
		previous = &stored[0]
	}
	holder := holdRuntimeCatalogWriter(t, store)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	released := make(chan struct{})
	var releaseErr error
	go func() {
		<-ctx.Done()
		releaseErr = holder.Rollback()
		close(released)
	}()
	t.Cleanup(func() {
		cancel()
		<-released
		if releaseErr != nil && !errors.Is(releaseErr, sql.ErrTxDone) {
			t.Error(releaseErr)
		}
	})
	started := time.Now()
	err = store.CompareServiceRuntime(ctx, before, next)
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		t.Logf("SQLite extended code=%d primary code=%d", sqliteErr.Code(), sqliteErr.Code()&0xff)
	}
	t.Logf("held catalog writer: elapsed=%s error=%v context=%v", time.Since(started), err, ctx.Err())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("runtime comparison did not wait for catalog writer admission until cancellation: %v", err)
	}
	if errors.Is(err, ErrAdvisoryWriteBusy) {
		t.Fatal("authoritative runtime contention became an advisory skip")
	}
	<-released
	assertRuntimeTransition(t, store, previous, revision)
	if err := store.CompareServiceRuntime(t.Context(), before, next); err != nil {
		t.Fatalf("comparison after catalog writer released: %v", err)
	}
	assertRuntimeTransition(t, store, next, revision+1)
}

func runtimeContentionTransition(t *testing.T, store *Store, name string) (*ServiceRuntime, *ServiceRuntime) {
	t.Helper()
	initial := runtimeTestRecord(t)
	if name == "insert" {
		return nil, &initial
	}
	if err := store.CompareServiceRuntime(t.Context(), nil, &initial); err != nil {
		t.Fatal(err)
	}
	next := initial
	next.Version++
	next.Failures = 2
	switch name {
	case "update":
		return &initial, &next
	case "delete":
		return &initial, nil
	case "confirm-insert":
		return nil, &initial
	case "confirm-update":
		if err := store.CompareServiceRuntime(t.Context(), &initial, &next); err != nil {
			t.Fatal(err)
		}
		return &initial, &next
	case "confirm-delete":
		if err := store.CompareServiceRuntime(t.Context(), &initial, nil); err != nil {
			t.Fatal(err)
		}
		return &initial, nil
	default:
		t.Fatalf("unknown transition %q", name)
		return nil, nil
	}
}

func holdRuntimeCatalogWriter(t *testing.T, store *Store) *sql.Tx {
	t.Helper()
	tx, err := store.db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	values, err := hostParams(testHost("runtime-catalog-writer"))
	if err != nil {
		t.Fatal(err)
	}
	// The same first write as ApplyHostChanges holds the database writer, not a runtime row lock.
	if _, err := dbsqlc.New(tx).UpsertHost(t.Context(), values); err != nil {
		t.Fatal(err)
	}
	// Warm a different pooled connection before starting the caller's deadline.
	// WAL readers must still see the committed catalog beside the held writer.
	if _, err := store.GetHost(t.Context(), "runtime-catalog-writer"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reader beside uncommitted catalog writer = %v", err)
	}
	return tx
}

func runtimeBarrierRevision(t *testing.T, store *Store) int {
	t.Helper()
	var revision int
	if err := store.db.QueryRowContext(t.Context(), "SELECT revision FROM service_runtime_barrier WHERE id=1").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func assertRuntimeTransition(t *testing.T, store *Store, next *ServiceRuntime, revision int) {
	t.Helper()
	got, err := store.GetServiceRuntime(t.Context(), "dev")
	if next == nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted runtime = %+v, %v", got, err)
	}
	if next != nil && (err != nil || !got.Equal(*next)) {
		t.Fatalf("persisted runtime = %+v, %v; want %+v", got, err, next)
	}
	if got := runtimeBarrierRevision(t, store); got != revision {
		t.Fatalf("barrier revision = %d, want %d", got, revision)
	}
}

func TestServiceRuntimeCatalogWriterRelease(t *testing.T) {
	for _, name := range []string{"insert", "update", "delete", "confirm-insert", "confirm-update", "confirm-delete"} {
		t.Run(name, func(t *testing.T) { runtimeCatalogWriterRelease(t, name) })
	}
}

func runtimeCatalogWriterRelease(t *testing.T, name string) {
	t.Helper()
	store := openTestStore(t)
	before, next := runtimeContentionTransition(t, store, name)
	revision := runtimeBarrierRevision(t, store)
	holder := holdRuntimeCatalogWriter(t, store)
	release := time.NewTimer(50 * time.Millisecond)
	defer release.Stop()
	released := make(chan error, 1)
	go func() {
		select {
		case <-release.C:
			released <- holder.Commit()
		case <-t.Context().Done():
			released <- holder.Rollback()
		}
	}()
	t.Cleanup(func() {
		if err := <-released; err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	})
	if err := store.CompareServiceRuntime(t.Context(), before, next); err != nil {
		t.Fatalf("runtime comparison beside a transient catalog writer: %v", err)
	}
	assertRuntimeTransition(t, store, next, revision+1)
	if _, err := store.GetHost(t.Context(), "runtime-catalog-writer"); err != nil {
		t.Fatalf("competing catalog writer did not commit: %v", err)
	}
}

func TestServiceRuntimeBarrierRollbackAndDurability(t *testing.T) {
	store := openTestStore(t)
	store.db.SetMaxOpenConns(1)
	record := runtimeTestRecord(t)
	if err := store.CompareServiceRuntime(t.Context(), nil, &record); err != nil {
		t.Fatal(err)
	}
	var synchronous int
	if err := store.db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 {
		t.Fatalf("runtime connection synchronous = %d, want FULL", synchronous)
	}
	revision := runtimeBarrierRevision(t, store)
	stale := record
	stale.Failures++
	if err := store.CompareServiceRuntime(t.Context(), &stale, nil); !errors.Is(err, ErrServiceRuntimeConflict) {
		t.Fatalf("incomplete before comparison = %v", err)
	}
	assertRuntimeTransition(t, store, &record, revision)
	if _, err := store.db.ExecContext(t.Context(), `CREATE TRIGGER reject_runtime_update
  BEFORE UPDATE ON service_runtime BEGIN SELECT RAISE(ABORT, 'runtime update rejected'); END`); err != nil {
		t.Fatal(err)
	}
	next := record
	next.Version++
	next.Failures++
	if err := store.CompareServiceRuntime(t.Context(), &record, &next); err == nil {
		t.Fatal("runtime update failure accepted")
	}
	assertRuntimeTransition(t, store, &record, revision)
}
