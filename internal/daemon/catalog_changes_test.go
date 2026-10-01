package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/worker"
)

func TestCatalogUnchangedPassDoesNotWriteSQLite(t *testing.T) {
	root := t.TempDir()
	writeCatalogMeta(t, root, "7K3D", catalogTestMeta("7K3D", worker.StateRunning, "boot-a"))
	database := filepath.Join(t.TempDir(), "catalog.db")
	store, err := storage.Open(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := catalogTestTime
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	catalog.now = func() time.Time { return now }
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(database + "-wal") //nolint:gosec // the database belongs to this test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(database + "-wal") //nolint:gosec // the database belongs to this test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("unchanged pass wrote SQLite: WAL grew from %d to %d bytes", len(before), len(after))
	}
}

func TestRunPersistsLastSeenAtOnShutdown(t *testing.T) {
	state := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var millis atomic.Int64
	millis.Store(catalogTestTime.UnixMilli())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{StateDir: state}, runOptions{
			now:               func() time.Time { return time.UnixMilli(millis.Load()) },
			bootID:            func() string { return "boot-a" },
			discoverSelf:      func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, nil },
			reconcileInterval: time.Hour,
		})
	}()
	conn := dialUnixRuntime(t, SocketPath(state))
	_ = conn.Close()
	want := catalogTestTime.Add(25 * time.Second)
	millis.Store(want.UnixMilli())
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.Context(), filepath.Join(state, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	hosts, err := store.ListHosts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || !hosts[0].LastSeenAt.Equal(want) {
		t.Fatalf("shutdown host observations = %+v, want last seen %v", hosts, want)
	}
}

func TestCatalogPublishesCommittedDiffAndSlowLiveness(t *testing.T) {
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateRunning, "boot-a")
	meta.CreatedAt = meta.CreatedAt.Add(123 * time.Nanosecond)
	writeCatalogMeta(t, root, meta.ID, meta)
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := catalogTestTime
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	catalog.now = func() time.Time { return now }
	var diffs []SessionDiff
	catalog.onChange = func(diff SessionDiff) {
		for _, group := range [][]storage.Session{diff.Added, diff.Changed} {
			for _, current := range group {
				persisted, err := store.GetSession(t.Context(), current.HostID, current.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(current, persisted) {
					t.Fatalf("diff record differs from committed row: diff=%+v row=%+v", current, persisted)
				}
			}
		}
		diffs = append(diffs, diff)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || len(diffs[0].Added) != 1 || diffs[0].Added[0].ID != "7K3D" {
		t.Fatalf("first diff = %+v", diffs)
	}
	diffs[0].Added[0].Command[0] = "subscriber-owned"
	now = now.Add(59 * time.Second)
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	host, err := store.GetHost(t.Context(), catalog.host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || !host.LastSeenAt.Equal(catalogTestTime) {
		t.Fatalf("unchanged pass emitted a diff or persisted liveness: diffs=%+v host=%+v", diffs, host)
	}
	attached := now
	meta.State, meta.LastAttachedAt = worker.StateDetached, &attached
	if err := worker.WriteMeta(filepath.Join(root, meta.ID), meta); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 || len(diffs[1].Changed) != 1 || diffs[1].Changed[0].State != storage.StateDetached || diffs[1].Changed[0].LastAttachedAt == nil {
		t.Fatalf("changed diff = %+v", diffs)
	}
	// Subscriber mutation cannot change the next diff's comparison baseline.
	*diffs[1].Changed[0].LastAttachedAt = catalogTestTime
	now = now.Add(time.Second)
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	host, err = store.GetHost(t.Context(), catalog.host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 || !host.LastSeenAt.Equal(now) {
		t.Fatalf("minute pass = diffs %+v host %+v", diffs, host)
	}
	if err := os.RemoveAll(filepath.Join(root, meta.ID)); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 3 || len(diffs[2].Changed) != 1 || diffs[2].Changed[0].State != storage.StateInterrupted {
		t.Fatalf("missing active session diff = %+v", diffs)
	}
	if err := catalog.Remove(t.Context(), storage.SessionID(meta.ID)); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 4 || len(diffs[3].Removed) != 1 || diffs[3].Removed[0] != "7K3D" {
		t.Fatalf("removed diff = %+v", diffs)
	}
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 4 {
		t.Fatalf("removed record returned in diff: %+v", diffs)
	}
}

func TestCatalogFailedPassDoesNotPublishOrAdvanceBaseline(t *testing.T) {
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateRunning, "boot-a")
	writeCatalogMeta(t, root, meta.ID, meta)
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	boundary := &receiptPublicationFailureStore{Store: store}
	catalog := newCatalogForTest(t, root, boundary, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	var diffs []SessionDiff
	catalog.onChange = func(diff SessionDiff) { diffs = append(diffs, diff) }
	if err := catalog.Reconcile(t.Context()); err == nil {
		t.Fatal("failed publication succeeded")
	}
	if len(diffs) != 0 || len(catalog.previous) != 0 || !catalog.persistedAt.IsZero() {
		t.Fatal("failed pass advanced the committed baseline")
	}
	catalog.store = store
	if err := catalog.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || len(diffs[0].Added) != 1 {
		t.Fatalf("retried pass lost added session: %+v", diffs)
	}
}

func TestStoredSessionComparisonCoversEveryColumn(t *testing.T) {
	attached, code := catalogTestTime.Add(time.Minute), 7
	original := storage.Session{ID: "7K3D", HostID: "host-a", Command: []string{"sh"}, Cwd: "/tmp", State: storage.StateExited, CreatedAt: catalogTestTime, LastAttachedAt: &attached, ExitCode: &code, LastOutputSequence: 12}
	for _, tc := range []struct {
		name   string
		change func(*storage.Session)
	}{
		{"ID", func(s *storage.Session) { s.ID = "TEST" }},
		{"host", func(s *storage.Session) { s.HostID = "host-b" }},
		{"command", func(s *storage.Session) { s.Command = []string{"sleep", "10"} }},
		{"cwd", func(s *storage.Session) { s.Cwd = "/work" }},
		{"state", func(s *storage.Session) { s.State = storage.StateInterrupted }},
		{"creation", func(s *storage.Session) { s.CreatedAt = s.CreatedAt.Add(time.Millisecond) }},
		{"attachment", func(s *storage.Session) { newer := attached.Add(time.Millisecond); s.LastAttachedAt = &newer }},
		{"missing attachment", func(s *storage.Session) { s.LastAttachedAt = nil }},
		{"exit", func(s *storage.Session) { changed := 9; s.ExitCode = &changed }},
		{"missing exit", func(s *storage.Session) { s.ExitCode = nil }},
		{"sequence", func(s *storage.Session) { s.LastOutputSequence++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := original
			tc.change(&changed)
			if sameStoredSession(original, changed) {
				t.Fatalf("changed %s was ignored", tc.name)
			}
		})
	}
	equivalent := original
	equivalent.CreatedAt = equivalent.CreatedAt.Add(time.Nanosecond).In(time.FixedZone("other", 3600))
	if !sameStoredSession(original, equivalent) {
		t.Fatal("equivalent SQLite timestamp was treated as changed")
	}
}
