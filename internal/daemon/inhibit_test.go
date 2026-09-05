package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/worker"
)

func TestSleepInhibitorFollowsReconciledWorkers(t *testing.T) {
	sessionsDir := t.TempDir()
	first := catalogTestMeta("H3TY", worker.StateRunning, "boot-a")
	second := catalogTestMeta("7K3D", worker.StateDetached, "boot-a")
	writeCatalogMeta(t, sessionsDir, first.ID, first)
	writeCatalogMeta(t, sessionsDir, second.ID, second)
	var updates []bool
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: sessionsDir,
		Host:        catalogTestHost(catalogTestTime),
		Store:       &catalogStoreStub{},
		Probe:       probeFunc(func(context.Context, string) error { return nil }),
		BootID:      func() string { return "boot-a" },
		Now:         func() time.Time { return catalogTestTime },
		OnReconcile: syncSleepInhibitor(func(active bool) { updates = append(updates, active) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	finishInhibitorTestWorker(t, sessionsDir, first)
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	finishInhibitorTestWorker(t, sessionsDir, second)
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updates, []bool{true, true, false}) {
		t.Fatalf("inhibitor updates = %v; detached workers must retain the lock", updates)
	}
}

func TestSleepInhibitorDoesNotTreatRebootedWorkerAsLive(t *testing.T) {
	sessionsDir := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateRunning, "old-boot")
	writeCatalogMeta(t, sessionsDir, meta.ID, meta)
	var updates []bool
	catalog := newCatalogForTest(t, sessionsDir, &catalogStoreStub{}, probeFunc(func(context.Context, string) error { return nil }), func() string { return "new-boot" })
	catalog.onReconcile = syncSleepInhibitor(func(active bool) { updates = append(updates, active) })
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updates, []bool{false}) {
		t.Fatalf("rebooted worker inhibitor updates = %v", updates)
	}
}

func TestSleepInhibitorPreservesLeaseAfterInconclusiveProbe(t *testing.T) {
	sessionsDir := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateRunning, "boot-a")
	writeCatalogMeta(t, sessionsDir, meta.ID, meta)
	var updates []bool
	var probeErr error
	catalog := newCatalogForTest(t, sessionsDir, &catalogStoreStub{}, probeFunc(func(context.Context, string) error { return probeErr }), func() string { return "boot-a" })
	catalog.onReconcile = syncSleepInhibitor(func(active bool) { updates = append(updates, active) })
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	probeErr = errors.New("socket probe timed out")
	if err := catalog.Reconcile(context.Background()); !errors.Is(err, probeErr) {
		t.Fatalf("Reconcile = %v", err)
	}
	if !reflect.DeepEqual(updates, []bool{true}) {
		t.Fatalf("inconclusive observation changed inhibitor: %v", updates)
	}
}

func finishInhibitorTestWorker(t *testing.T, sessionsDir string, meta worker.Meta) {
	t.Helper()
	exitedAt := catalogTestTime.Add(time.Minute)
	exitCode := 0
	meta.State = worker.StateExited
	meta.ExitedAt = &exitedAt
	meta.ExitCode = &exitCode
	if err := worker.WriteMeta(filepath.Join(sessionsDir, meta.ID), meta); err != nil {
		t.Fatal(err)
	}
}
