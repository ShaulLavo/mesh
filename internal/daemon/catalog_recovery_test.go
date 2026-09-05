package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

func TestCatalogRetainsRecoveryUntilExplicitlyForgotten(t *testing.T) {
	for _, test := range []struct {
		name, file, recoveredFrom string
		mode                      os.FileMode
	}{
		{name: "checkpoint", file: "recovery.json", mode: 0o600},
		{name: "unreadable checkpoint", file: "recovery.json"},
		{name: "source intent", file: "recovery-intent.json", mode: 0o600},
		{name: "explicit recipe", file: "recovery-command.json", mode: 0o600},
		{name: "selected source before intent", file: "recovery.lock", mode: 0o600},
		{name: "replacement without checkpoint", recoveredFrom: "Q8ME"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testRecoveryRetention(t, test.file, test.mode, test.recoveredFrom)
		})
	}
}

func testRecoveryRetention(t *testing.T, file string, mode os.FileMode, recoveredFrom string) {
	t.Helper()
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateExited, "boot-a")
	meta.CreatedAt = catalogTestTime.Add(-30 * 24 * time.Hour)
	exitedAt, exitCode := meta.CreatedAt.Add(time.Second), 0
	meta.ExitedAt, meta.ExitCode, meta.RecoveredFrom = &exitedAt, &exitCode, recoveredFrom
	writeCatalogMeta(t, root, meta.ID, meta)
	dir := filepath.Join(root, meta.ID)
	if file != "" {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("unsupported or damaged saved data"), mode); err != nil {
			t.Fatal(err)
		}
	}
	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(store.retired) != 0 || len(store.observed) != 1 {
		t.Fatalf("saved recovery was automatically pruned: retired=%v observed=%v", store.retired, store.observed)
	}
	if _, err := os.Stat(paths.Meta(dir)); err != nil {
		t.Fatalf("retained metadata is missing: %v", err)
	}
	if err := os.WriteFile(paths.Forgotten(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(store.retired) != 1 || store.retired[0] != storage.SessionID(meta.ID) {
		t.Fatalf("explicitly forgotten recovery was not retired: %v", store.retired)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicitly forgotten recovery directory survived: %v", err)
	}
}

func TestCatalogRetentionCountExcludesRecoveryAttempts(t *testing.T) {
	for _, test := range []struct {
		name                       string
		protected, legacy, retired int
	}{
		{name: "saved attempts do not evict legacy", protected: maxRetainedTerminalSessions + 2, legacy: 2},
		{name: "legacy history still capped", protected: 2, legacy: maxRetainedTerminalSessions + 2, retired: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			testRecoveryRetentionCount(t, test.protected, test.legacy, test.retired)
		})
	}
}

func testRecoveryRetentionCount(t *testing.T, protected, legacy, wantRetired int) {
	t.Helper()
	root := t.TempDir()
	catalog := &Catalog{sessionsDir: root, now: func() time.Time { return catalogTestTime }}
	var observed []storage.Session
	for index := range protected + legacy {
		id := fmt.Sprintf("%04d", index)
		dir := filepath.Join(root, id)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		observed = append(observed, storage.Session{ID: storage.SessionID(id), State: storage.StateExited,
			CreatedAt: catalogTestTime.Add(-time.Duration(protected+legacy-index) * time.Minute)})
		if index >= protected {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "recovery-intent.json"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	kept, retired := catalog.retireFinishedSessions(observed)
	if len(retired) != wantRetired || len(kept) != len(observed)-wantRetired {
		t.Fatalf("kept=%d retired=%v, want %d retired legacy sessions", len(kept), retired, wantRetired)
	}
	for index, id := range retired {
		if want := storage.SessionID(fmt.Sprintf("%04d", protected+index)); id != want {
			t.Fatalf("retired %s, want legacy session %s", id, want)
		}
	}
}

func TestCatalogRetentionPreservesInaccessibleRecoveryDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "7K3D")
	if err := os.Mkdir(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restore owner traversal on the temporary test directory
	catalog := &Catalog{sessionsDir: root, now: func() time.Time { return catalogTestTime }}
	observed := []storage.Session{{ID: "7K3D", State: storage.StateExited, CreatedAt: catalogTestTime.Add(-30 * 24 * time.Hour)}}
	kept, retired := catalog.retireFinishedSessions(observed)
	if len(kept) != 1 || len(retired) != 0 {
		t.Fatalf("unreadable recovery directory was pruned: kept=%v retired=%v", kept, retired)
	}
}

func TestCatalogReconcileRetainsRecoveryStartedAfterPruningSelection(t *testing.T) {
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateExited, "boot-a")
	meta.CreatedAt, meta.Cwd = catalogTestTime.Add(-30*24*time.Hour), root
	exitedAt, code := meta.CreatedAt.Add(time.Second), 0
	meta.ExitedAt, meta.ExitCode = &exitedAt, &code
	writeCatalogMeta(t, root, meta.ID, meta)
	store := &recoveryDuringReconcileStore{}
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	store.during = func(ctx context.Context) error {
		store.during = nil
		cfg := recovery.Config{SessionsDir: root, HostID: string(catalog.host.ID), Runtime: worker.RecoveryRuntime{
			SessionsDir: root, HostID: string(catalog.host.ID), Executable: filepath.Join(root, "unavailable-mesh"),
		}}
		_, err := recovery.Recover(ctx, cfg, recovery.Request{SessionID: meta.ID})
		var failed *recovery.LaunchFailure
		if !errors.As(err, &failed) {
			return fmt.Errorf("expected a proven recovery launch failure, got %w", err)
		}
		return nil
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, meta.ID)
	for _, name := range []string{"meta.json", "recovery-intent.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("cleanup removed recovery started during reconciliation: %s: %v", name, err)
		}
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(store.observed) != 1 || store.observed[0].ID != storage.SessionID(meta.ID) {
		t.Fatalf("next reconciliation did not restore the retained source: %v", store.observed)
	}
}

type recoveryDuringReconcileStore struct {
	catalogStoreStub
	during func(context.Context) error
}

func (s *recoveryDuringReconcileStore) ReconcileHost(ctx context.Context, host storage.Host, sessions []storage.Session) error {
	if err := s.catalogStoreStub.ReconcileHost(ctx, host, sessions); err != nil {
		return err
	}
	if s.during == nil {
		return nil
	}
	return s.during(ctx)
}
