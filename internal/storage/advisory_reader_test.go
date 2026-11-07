package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCatalogReviewAdvisoryReadKeepsShortDeadline(t *testing.T) {
	store, err := OpenAdvisory(t.Context(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	holder, err := store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.ExecContext(t.Context(), "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(t.Context(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = holder.ExecContext(context.WithoutCancel(t.Context()), "ROLLBACK") })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = store.ListHostSessions(ctx, "host-review")
	elapsed := time.Since(start)
	t.Logf("actual advisory reader elapsed=%v err=%v context=%v", elapsed, err, ctx.Err())
	if elapsed > 400*time.Millisecond {
		t.Errorf("advisory reader exceeded unchanged 200 ms cap: %v", elapsed)
	}
}

func TestCatalogReviewNewWriterConnectionExhaustionIsAdvisory(t *testing.T) {
	store, err := OpenAdvisory(t.Context(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	holder, err := store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.ExecContext(t.Context(), "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(t.Context(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = holder.ExecContext(context.WithoutCancel(t.Context()), "ROLLBACK") })
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	started := time.Now()
	tx, release, err := store.beginCatalogWrite(ctx)
	if release != nil {
		defer release()
	}
	if tx != nil {
		_ = tx.Rollback()
		t.Fatal("exclusive owner failed to hold write admission")
	}
	if !errors.Is(err, ErrAdvisoryWriteBusy) || ctx.Err() != nil {
		t.Fatalf("new connection exposed admission BUSY as a different error: %v, context=%v", err, ctx.Err())
	}
	if elapsed := time.Since(started); elapsed < 4800*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("writer did not use bounded five-second admission: %s", elapsed)
	}
}

func TestCatalogReviewPoolAdmissionSharesWriterBudget(t *testing.T) {
	store, err := OpenAdvisory(t.Context(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for range sqliteMaxOpenConns {
		conn, err := store.db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	started := time.Now()
	tx, release, err := store.beginCatalogWrite(ctx)
	if tx != nil {
		_ = tx.Rollback()
	}
	if release != nil {
		release()
	}
	if !errors.Is(err, ErrAdvisoryWriteBusy) || ctx.Err() != nil {
		t.Fatalf("pool admission escaped its writer budget: error=%v context=%v", err, ctx.Err())
	}
	if elapsed := time.Since(started); elapsed < 4800*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("pool admission budget=%s", elapsed)
	}
}
