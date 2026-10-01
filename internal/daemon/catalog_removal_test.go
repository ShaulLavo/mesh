package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

type removalRaceStore struct {
	*storage.Store
	scanned chan struct{}
	publish chan struct{}
	pause   sync.Once
}

func (s *removalRaceStore) ApplyHostChanges(ctx context.Context, host storage.HostID, changes storage.HostChanges) error {
	if s.scanned != nil {
		s.pause.Do(func() {
			close(s.scanned)
			<-s.publish
		})
	}
	if err := s.Store.ApplyHostChanges(ctx, host, changes); err != nil {
		return fmt.Errorf("publish reconciliation snapshot: %w", err)
	}
	return nil
}

func (s *removalRaceStore) RetireSessions(ctx context.Context, host storage.HostID, ids []storage.SessionID) (int64, error) {
	if waiting, ok := ctx.(*removalWaitContext); ok {
		// Only catalog gate waits count, not the driver's context checks.
		waiting.driver.Store(true)
		defer waiting.driver.Store(false)
	}
	removed, err := s.Store.RetireSessions(ctx, host, ids)
	if err != nil {
		return 0, fmt.Errorf("retire session rows: %w", err)
	}
	return removed, nil
}

type removalWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
	driver  atomic.Bool
}

func (c *removalWaitContext) Done() <-chan struct{} {
	if !c.driver.Load() {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.Context.Done()
}

func TestCatalogRemovalCannotBeUndoneByAnInFlightScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateExited, "boot-a")
	code, exitedAt := 7, catalogTestTime.Add(time.Second)
	meta.ExitCode, meta.ExitedAt = &code, &exitedAt
	writeCatalogMeta(t, root, meta.ID, meta)
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	paused := &removalRaceStore{Store: store}
	catalog := newCatalogForTest(t, root, paused,
		probeFunc(func(context.Context, string) error { return syscall.ENOENT }),
		func() string { return "boot-a" })
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	meta.Cwd = root
	if err := worker.WriteMeta(filepath.Join(root, meta.ID), meta); err != nil {
		t.Fatal(err)
	}
	paused.scanned, paused.publish = make(chan struct{}), make(chan struct{})
	var release sync.Once
	publish := func() { release.Do(func() { close(paused.publish) }) }
	t.Cleanup(publish)
	reconciled := make(chan error, 1)
	go func() { reconciled <- catalog.Reconcile(t.Context()) }()
	waitSignal(t, paused.scanned, "reconciliation snapshot")
	ctx := &removalWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	removed := make(chan error, 1)
	go func() { removed <- catalog.Remove(ctx, storage.SessionID(meta.ID)) }()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatal(err)
		}
		publish()
	case <-ctx.waiting:
		publish()
		if err := waitServerResult(t, removed, "session removal"); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("removal neither completed nor waited for reconciliation")
	}
	if err := waitServerResult(t, reconciled, "reconciliation publication"); err != nil {
		t.Fatal(err)
	}
	rows, err := catalog.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("reconciliation resurrected removed session: %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(root, meta.ID)); !os.IsNotExist(err) {
		t.Fatalf("removed session directory survived: %v", err)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err = catalog.List(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("removed session returned on the next scan: %+v, %v", rows, err)
	}
}
