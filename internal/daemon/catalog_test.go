package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

var catalogTestTime = time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)

func TestNewCatalogValidatesConfig(t *testing.T) {
	empty := ""
	valid := CatalogConfig{
		SessionsDir: t.TempDir(),
		Host:        catalogTestHost(catalogTestTime),
		Store:       &catalogStoreStub{},
		Probe:       probeFunc(func(context.Context, string) error { return nil }),
		BootID:      func() string { return "boot-a" },
	}

	tests := []struct {
		name   string
		mutate func(*CatalogConfig)
	}{
		{name: "empty sessions directory", mutate: func(cfg *CatalogConfig) { cfg.SessionsDir = "" }},
		{name: "empty host ID", mutate: func(cfg *CatalogConfig) { cfg.Host.ID = " " }},
		{name: "empty Mesh identity", mutate: func(cfg *CatalogConfig) { cfg.Host.MeshIdentity = " " }},
		{name: "zero last seen time", mutate: func(cfg *CatalogConfig) { cfg.Host.LastSeenAt = time.Time{} }},
		{name: "pre-epoch last seen time", mutate: func(cfg *CatalogConfig) { cfg.Host.LastSeenAt = time.UnixMilli(-1) }},
		{name: "empty alias", mutate: func(cfg *CatalogConfig) { cfg.Host.Alias = &empty }},
		{name: "empty Tailscale name", mutate: func(cfg *CatalogConfig) { cfg.Host.TailscaleName = &empty }},
		{name: "nil store", mutate: func(cfg *CatalogConfig) { cfg.Store = nil }},
		{name: "nil probe", mutate: func(cfg *CatalogConfig) { cfg.Probe = nil }},
		{name: "nil boot ID reader", mutate: func(cfg *CatalogConfig) { cfg.BootID = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			if _, err := NewCatalog(cfg); err == nil {
				t.Fatal("NewCatalog() error = nil")
			}
		})
	}

	if _, err := NewCatalog(valid); err != nil {
		t.Fatalf("NewCatalog(valid config): %v", err)
	}
}

func TestCatalogReconcileMapsWorkerState(t *testing.T) {
	dead := syscall.ECONNREFUSED
	exitCode := 7
	exitedAt := catalogTestTime.Add(time.Minute)

	tests := []struct {
		name        string
		meta        worker.Meta
		currentBoot string
		probeErr    error
		wantState   storage.SessionState
		wantProbes  int
	}{
		{
			name:        "responsive running worker",
			meta:        catalogTestMeta("11VE", worker.StateRunning, "boot-a"),
			currentBoot: "boot-a",
			wantState:   storage.StateRunning,
			wantProbes:  1,
		},
		{
			name:        "unresponsive running worker",
			meta:        catalogTestMeta("DEAD", worker.StateRunning, "boot-a"),
			currentBoot: "boot-a",
			probeErr:    dead,
			wantState:   storage.StateInterrupted,
			wantProbes:  1,
		},
		{
			name:        "worker from an earlier boot",
			meta:        catalogTestMeta("RB07", worker.StateRunning, "boot-a"),
			currentBoot: "boot-b",
			wantState:   storage.StateInterrupted,
			wantProbes:  0,
		},
		{
			name: "exited worker",
			meta: func() worker.Meta {
				meta := catalogTestMeta("EX17", worker.StateExited, "boot-a")
				meta.ExitedAt = &exitedAt
				meta.ExitCode = &exitCode
				return meta
			}(),
			currentBoot: "boot-b",
			wantState:   storage.StateExited,
			wantProbes:  0,
		},
		{
			name:        "boot identity unavailable",
			meta:        catalogTestMeta("N0B1", worker.StateRunning, ""),
			currentBoot: "boot-a",
			wantState:   storage.StateRunning,
			wantProbes:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionsDir := t.TempDir()
			writeCatalogMeta(t, sessionsDir, tt.meta.ID, tt.meta)

			var probePaths []string
			probe := probeFunc(func(_ context.Context, path string) error {
				probePaths = append(probePaths, path)
				return tt.probeErr
			})
			store := &catalogStoreStub{}
			catalog := newCatalogForTest(t, sessionsDir, store, probe, func() string { return tt.currentBoot })

			if err := catalog.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			if store.upsertCalls != 1 || store.reconcileCalls != 1 {
				t.Fatalf("store calls = upsert %d, reconcile %d; want 1, 1", store.upsertCalls, store.reconcileCalls)
			}
			if got := store.events; !reflect.DeepEqual(got, []string{"reconcile-host"}) {
				t.Fatalf("store calls = %v, want one atomic reconciliation", got)
			}
			if len(store.observed) != 1 {
				t.Fatalf("observed sessions = %d, want 1", len(store.observed))
			}
			got := store.observed[0]
			if got.ID != storage.SessionID(tt.meta.ID) || got.HostID != catalog.host.ID {
				t.Fatalf("observed identity = %s/%s, want %s/%s", got.HostID, got.ID, catalog.host.ID, tt.meta.ID)
			}
			if got.State != tt.wantState {
				t.Fatalf("state = %q, want %q", got.State, tt.wantState)
			}
			if !reflect.DeepEqual(got.Command, tt.meta.Command) || got.Cwd != tt.meta.Cwd || !got.CreatedAt.Equal(tt.meta.CreatedAt) {
				t.Fatalf("observed metadata = %#v, want values from %#v", got, tt.meta)
			}
			if got.LastAttachedAt != nil || got.LastOutputSequence != 0 {
				t.Fatalf("derived fields = attached %v, sequence %d; want nil, 0", got.LastAttachedAt, got.LastOutputSequence)
			}
			if !equalInt(got.ExitCode, tt.meta.ExitCode) {
				t.Fatalf("exit code = %v, want %v", got.ExitCode, tt.meta.ExitCode)
			}
			if len(probePaths) != tt.wantProbes {
				t.Fatalf("probe calls = %d, want %d", len(probePaths), tt.wantProbes)
			}
			if tt.wantProbes == 1 {
				wantPath := filepath.Join(sessionsDir, tt.meta.ID, "sock")
				if probePaths[0] != wantPath {
					t.Fatalf("probe path = %q, want %q", probePaths[0], wantPath)
				}
			}
		})
	}
}

func TestCatalogReconcileRejectsIncompleteScanBeforeMutation(t *testing.T) {
	exitCode := 0
	exitedAt := catalogTestTime.Add(time.Minute)

	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "directory and metadata IDs differ",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, "DIRNAME", catalogTestMeta("OTHER", worker.StateRunning, "boot-a"))
			},
		},
		{
			name: "unsupported worker state",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, "4N0W", catalogTestMeta("4N0W", "paused", "boot-a"))
			},
		},
		{
			name: "worker-derived interrupted state",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, "1N7R", catalogTestMeta("1N7R", worker.StateInterrupted, "boot-a"))
			},
		},
		{
			name: "empty command",
			setup: func(t *testing.T, root string) {
				t.Helper()
				meta := catalogTestMeta("C0D0", worker.StateRunning, "boot-a")
				meta.Command = nil
				writeCatalogMeta(t, root, meta.ID, meta)
			},
		},
		{
			name: "zero PID",
			setup: func(t *testing.T, root string) {
				t.Helper()
				meta := catalogTestMeta("P1D0", worker.StateRunning, "boot-a")
				meta.PID = 0
				writeCatalogMeta(t, root, meta.ID, meta)
			},
		},
		{
			name: "zero creation time",
			setup: func(t *testing.T, root string) {
				t.Helper()
				meta := catalogTestMeta("71ME", worker.StateRunning, "boot-a")
				meta.CreatedAt = time.Time{}
				writeCatalogMeta(t, root, meta.ID, meta)
			},
		},
		{
			name: "running worker with exit fields",
			setup: func(t *testing.T, root string) {
				t.Helper()
				meta := catalogTestMeta("R4NE", worker.StateRunning, "boot-a")
				meta.ExitedAt = &exitedAt
				meta.ExitCode = &exitCode
				writeCatalogMeta(t, root, meta.ID, meta)
			},
		},
		{
			name: "exited worker without exit fields",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, "N0EX", catalogTestMeta("N0EX", worker.StateExited, "boot-a"))
			},
		},
		{
			name: "session ID exceeds wire width",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, "NINECHARS", catalogTestMeta("NINECHARS", worker.StateRunning, "boot-a"))
			},
		},
		{
			name: "session ID is not canonical Crockford base32",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, "7k3d", catalogTestMeta("7k3d", worker.StateRunning, "boot-a"))
			},
		},
		{
			name: "blank session ID",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, " ", catalogTestMeta(" ", worker.StateRunning, "boot-a"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionsDir := t.TempDir()
			tt.setup(t, sessionsDir)
			store := &catalogStoreStub{}
			catalog := newCatalogForTest(t, sessionsDir, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })

			if err := catalog.Reconcile(context.Background()); err == nil {
				t.Fatal("Reconcile() error = nil")
			}
			if store.upsertCalls != 0 || store.reconcileCalls != 0 {
				t.Fatalf("store calls = upsert %d, reconcile %d; want 0, 0", store.upsertCalls, store.reconcileCalls)
			}
		})
	}
}

func TestCatalogReconcileSortsObservations(t *testing.T) {
	sessionsDir := t.TempDir()
	writeCatalogMeta(t, sessionsDir, "Z9D9", catalogTestMeta("Z9D9", worker.StateRunning, "boot-a"))
	writeCatalogMeta(t, sessionsDir, "A1FA", catalogTestMeta("A1FA", worker.StateRunning, "boot-a"))
	writeCatalogMeta(t, sessionsDir, "M1D5", catalogTestMeta("M1D5", worker.StateRunning, "boot-a"))

	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, sessionsDir, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}

	got := make([]storage.SessionID, len(store.observed))
	for i := range store.observed {
		got[i] = store.observed[i].ID
	}
	want := []storage.SessionID{"A1FA", "M1D5", "Z9D9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observation order = %v, want %v", got, want)
	}
}

func TestCatalogReconcileCancellationDuringProbeDoesNotWrite(t *testing.T) {
	sessionsDir := t.TempDir()
	writeCatalogMeta(t, sessionsDir, "CA2C", catalogTestMeta("CA2C", worker.StateRunning, "boot-a"))

	ctx, cancel := context.WithCancel(context.Background())
	store := &catalogStoreStub{}
	probe := probeFunc(func(context.Context, string) error {
		cancel()
		return context.Canceled
	})
	catalog := newCatalogForTest(t, sessionsDir, store, probe, func() string { return "boot-a" })

	err := catalog.Reconcile(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() error = %v, want context.Canceled", err)
	}
	if store.upsertCalls != 0 || store.reconcileCalls != 0 {
		t.Fatalf("store calls = upsert %d, reconcile %d; want 0, 0", store.upsertCalls, store.reconcileCalls)
	}
}

func TestCatalogReconcileCancellationAfterEmptyScanDoesNotWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &catalogStoreStub{}
	catalog := newCatalogForTest(
		t,
		t.TempDir(),
		store,
		probeFunc(func(context.Context, string) error { return nil }),
		func() string {
			cancel()
			return "boot-a"
		},
	)

	err := catalog.Reconcile(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() error = %v, want context.Canceled", err)
	}
	if store.upsertCalls != 0 || store.reconcileCalls != 0 {
		t.Fatalf("store calls = upsert %d, reconcile %d; want 0, 0", store.upsertCalls, store.reconcileCalls)
	}
}

func TestCatalogReconcileConvergesInSQLite(t *testing.T) {
	ctx := context.Background()
	sessionsDir := t.TempDir()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	liveMeta := catalogTestMeta("11VE", worker.StateRunning, "boot-a")
	missingMeta := catalogTestMeta("M155", worker.StateRunning, "boot-a")
	writeCatalogMeta(t, sessionsDir, liveMeta.ID, liveMeta)
	writeCatalogMeta(t, sessionsDir, missingMeta.ID, missingMeta)
	catalog := newCatalogForTest(t, sessionsDir, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })

	if err := catalog.Reconcile(ctx); err != nil {
		t.Fatalf("initial Reconcile(): %v", err)
	}
	attachedAt := catalogTestTime.Add(2 * time.Minute)
	live, err := store.GetSession(ctx, catalog.host.ID, storage.SessionID(liveMeta.ID))
	if err != nil {
		t.Fatal(err)
	}
	live.LastAttachedAt = &attachedAt
	live.LastOutputSequence = 4096
	if _, err := store.UpsertSession(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(sessionsDir, missingMeta.ID)); err != nil {
		t.Fatal(err)
	}

	if err := catalog.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile(): %v", err)
	}
	gotLive, err := catalog.Get(ctx, storage.SessionID(liveMeta.ID))
	if err != nil {
		t.Fatal(err)
	}
	if gotLive.State != storage.StateRunning || gotLive.LastAttachedAt == nil || !gotLive.LastAttachedAt.Equal(attachedAt) || gotLive.LastOutputSequence != 4096 {
		t.Fatalf("live session after reconciliation = %#v", gotLive)
	}
	gotMissing, err := catalog.Get(ctx, storage.SessionID(missingMeta.ID))
	if err != nil {
		t.Fatal(err)
	}
	if gotMissing.State != storage.StateInterrupted {
		t.Fatalf("missing session state = %q, want %q", gotMissing.State, storage.StateInterrupted)
	}

	before, err := catalog.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(ctx); err != nil {
		t.Fatalf("repeat Reconcile(): %v", err)
	}
	after, err := catalog.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("repeat reconciliation changed sessions:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

// A scan the daemon cannot trust must leave SQLite exactly as it found it,
// rather than committing a partial view that would mark live sessions dead.
// The trigger here is a record that decodes but contradicts itself: unlike a
// torn write, which is quarantined, that means something wrote a session
// directory Mesh does not understand, and guessing is not safe.
func TestCatalogBrokenScanPreservesSQLiteState(t *testing.T) {
	ctx := context.Background()
	sessionsDir := t.TempDir()
	contradictory := catalogTestMeta("BR0K", worker.StateExited, "boot-a")
	writeCatalogMeta(t, sessionsDir, contradictory.ID, contradictory)
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	originalHost := catalogTestHost(catalogTestTime)
	if _, err := store.UpsertHost(ctx, originalHost); err != nil {
		t.Fatal(err)
	}
	originalSession := storage.Session{
		ID:        "KNOWN",
		HostID:    originalHost.ID,
		Command:   []string{"sh"},
		Cwd:       "/tmp",
		State:     storage.StateRunning,
		CreatedAt: catalogTestTime,
	}
	if _, err := store.UpsertSession(ctx, originalSession); err != nil {
		t.Fatal(err)
	}

	newHost := originalHost
	newHost.LastSeenAt = originalHost.LastSeenAt.Add(time.Hour)
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: sessionsDir,
		Host:        newHost,
		Store:       store,
		Probe:       probeFunc(func(context.Context, string) error { return nil }),
		BootID:      func() string { return "boot-a" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile() error = nil")
	}

	gotHost, err := store.GetHost(ctx, originalHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !gotHost.LastSeenAt.Equal(originalHost.LastSeenAt) {
		t.Fatalf("last seen time = %v, want unchanged %v", gotHost.LastSeenAt, originalHost.LastSeenAt)
	}
	gotSession, err := store.GetSession(ctx, originalHost.ID, originalSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSession.State != storage.StateRunning {
		t.Fatalf("known session state = %q, want %q", gotSession.State, storage.StateRunning)
	}
}

func TestCatalogListAndGetValidateContextAndID(t *testing.T) {
	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, t.TempDir(), store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })

	if _, err := catalog.List(nil); err == nil { //nolint:staticcheck // boundary test intentionally passes a nil context
		t.Fatal("List(nil) error = nil")
	}
	if _, err := catalog.Get(context.Background(), " "); err == nil {
		t.Fatal("Get(empty ID) error = nil")
	}
	if store.listCalls != 0 || store.getCalls != 0 {
		t.Fatalf("store calls = list %d, get %d; want 0, 0", store.listCalls, store.getCalls)
	}
}

type probeFunc func(context.Context, string) error

func (f probeFunc) Probe(ctx context.Context, path string) error { return f(ctx, path) }

type catalogStoreStub struct {
	upsertCalls    int
	reconcileCalls int
	listCalls      int
	getCalls       int
	events         []string
	host           storage.Host
	observed       []storage.Session
	retired        []storage.SessionID
}

func (s *catalogStoreStub) ReconcileHost(_ context.Context, host storage.Host, observed []storage.Session) error {
	s.upsertCalls++
	s.reconcileCalls++
	s.events = append(s.events, "reconcile-host")
	s.host = cloneHost(host)
	s.observed = append([]storage.Session(nil), observed...)
	return nil
}

func (s *catalogStoreStub) ListHostSessions(context.Context, storage.HostID) ([]storage.Session, error) {
	s.listCalls++
	return append([]storage.Session(nil), s.observed...), nil
}

func (s *catalogStoreStub) GetSession(_ context.Context, hostID storage.HostID, sessionID storage.SessionID) (storage.Session, error) {
	s.getCalls++
	return storage.Session{HostID: hostID, ID: sessionID}, nil
}

func newCatalogForTest(t *testing.T, sessionsDir string, store CatalogStore, probe WorkerProbe, bootID func() string) *Catalog {
	t.Helper()
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: sessionsDir,
		Host:        catalogTestHost(catalogTestTime),
		Store:       store,
		Probe:       probe,
		BootID:      bootID,
		Now:         func() time.Time { return catalogTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestCatalogReconcileRefreshesHostObservationTime(t *testing.T) {
	store := &catalogStoreStub{}
	want := catalogTestTime.Add(90 * time.Minute)
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: t.TempDir(),
		Host:        catalogTestHost(catalogTestTime),
		Store:       store,
		Probe:       probeFunc(func(context.Context, string) error { return nil }),
		BootID:      func() string { return "boot-a" },
		Now:         func() time.Time { return want },
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.host.LastSeenAt.Equal(want) {
		t.Fatalf("last seen time = %v, want %v", store.host.LastSeenAt, want)
	}
}

func catalogTestHost(lastSeenAt time.Time) storage.Host {
	alias := "local"
	tailscaleName := "host.example.ts.net"
	return storage.Host{
		ID:            "host-id",
		Alias:         &alias,
		MeshIdentity:  "mesh-identity",
		TailscaleName: &tailscaleName,
		LastSeenAt:    lastSeenAt,
	}
}

func catalogTestMeta(id, state, bootID string) worker.Meta {
	return worker.Meta{
		ID:        id,
		PID:       1234,
		Command:   []string{"sh", "-c", "printf hello"},
		Cwd:       "/tmp/project",
		State:     state,
		CreatedAt: catalogTestTime,
		BootID:    bootID,
	}
}

func writeCatalogMeta(t *testing.T, root, directory string, meta worker.Meta) {
	t.Helper()
	dir := filepath.Join(root, directory)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
}

func equalInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func TestCatalogConstructorCopiesHostStrings(t *testing.T) {
	host := catalogTestHost(catalogTestTime)
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: t.TempDir(),
		Host:        host,
		Store:       &catalogStoreStub{},
		Probe:       probeFunc(func(context.Context, string) error { return nil }),
		BootID:      func() string { return "boot-a" },
	})
	if err != nil {
		t.Fatal(err)
	}
	*host.Alias = "changed"
	*host.TailscaleName = "changed.example.ts.net"
	if *catalog.host.Alias != "local" || *catalog.host.TailscaleName != "host.example.ts.net" {
		t.Fatalf("catalog host changed through caller pointers: %#v", catalog.host)
	}
}

func TestCatalogGetRejectsWireInvalidID(t *testing.T) {
	catalog := newCatalogForTest(t, t.TempDir(), &catalogStoreStub{}, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	for _, id := range []storage.SessionID{storage.SessionID(strings.Repeat("x", 9)), "7k3d", "ABID"} {
		if _, err := catalog.Get(context.Background(), id); err == nil {
			t.Errorf("Get(%q) error = nil", id)
		}
	}
}

func TestCatalogReconcileIgnoresWorkerStillPublishingState(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "7K3D")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Launching(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &catalogStoreStub{}
	probeCalls := 0
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error {
		probeCalls++
		return nil
	}), func() string { return "boot-a" })

	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probeCalls != 0 || len(store.observed) != 0 {
		t.Fatalf("publishing worker was probed %d times and observed as %#v", probeCalls, store.observed)
	}
}

func TestCatalogRereadsExitedMetadataAfterWorkerBecomesUnavailable(t *testing.T) {
	root := t.TempDir()
	meta := catalogTestMeta("7K3D", worker.StateRunning, "boot-a")
	writeCatalogMeta(t, root, meta.ID, meta)
	exitCode := 7
	exitedAt := meta.CreatedAt.Add(time.Second)
	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error {
		meta.State = worker.StateExited
		meta.ExitCode = &exitCode
		meta.ExitedAt = &exitedAt
		if err := worker.WriteMeta(filepath.Join(root, meta.ID), meta); err != nil {
			t.Fatal(err)
		}
		return syscall.ECONNREFUSED
	}), func() string { return "boot-a" })

	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.observed) != 1 || store.observed[0].State != storage.StateExited || store.observed[0].ExitCode == nil || *store.observed[0].ExitCode != exitCode {
		t.Fatalf("clean exit observation = %#v", store.observed)
	}
}

func TestCatalogRejectsInconclusiveProbeBeforeMutation(t *testing.T) {
	root := t.TempDir()
	meta := catalogTestMeta("9M2Q", worker.StateRunning, "boot-a")
	writeCatalogMeta(t, root, meta.ID, meta)
	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error {
		return syscall.EACCES
	}), func() string { return "boot-a" })

	err := catalog.Reconcile(context.Background())
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("Reconcile error = %v, want EACCES", err)
	}
	if store.upsertCalls != 0 || store.reconcileCalls != 0 {
		t.Fatalf("inconclusive probe mutated store: upsert %d reconcile %d", store.upsertCalls, store.reconcileCalls)
	}
}

// A directory holding neither a launching marker nor metadata belongs to a
// create still in flight: LaunchDetached makes the directory before it writes
// the marker, so a concurrent reconcile sees that gap. Treating it as fatal
// failed the racing client's publish and left it an unnamed live session.
func TestCatalogSkipsDirectoryReservedButNotYetMarked(t *testing.T) {
	sessionsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(sessionsDir, "MISSING"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCatalogMeta(t, sessionsDir, "H3TY", catalogTestMeta("H3TY", worker.StateRunning, "boot-a"))

	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, sessionsDir, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v; a half-reserved directory must not fail the scan", err)
	}
	if store.reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", store.reconcileCalls)
	}
	if len(store.observed) != 1 || store.observed[0].ID != "H3TY" {
		t.Fatalf("observed sessions = %+v; want only the published session", store.observed)
	}
}

// A torn meta.json is what power loss during a worker's exit record looks like.
// The daemon's startup Reconcile runs before the Unix socket, the tailnet
// listener and the SSH listener exist, so failing here bricked the whole host
// on every boot with no way in to repair it. Quarantine the one damaged session
// as interrupted instead, and keep coordinating the healthy ones.
func TestCatalogQuarantinesTornMetadataInsteadOfFailingStartup(t *testing.T) {
	sessionsDir := t.TempDir()
	broken := filepath.Join(sessionsDir, "BR0K")
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	// A zero-length file is precisely what a rename without a preceding sync
	// leaves behind.
	if err := os.WriteFile(filepath.Join(broken, "meta.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeCatalogMeta(t, sessionsDir, "H3TY", catalogTestMeta("H3TY", worker.StateRunning, "boot-a"))

	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, sessionsDir, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v; one torn record must not stop the daemon", err)
	}

	var quarantined, healthy bool
	for _, got := range store.observed {
		switch got.ID {
		case "BR0K":
			quarantined = true
			if got.State != storage.StateInterrupted {
				t.Fatalf("quarantined session state = %q, want %q", got.State, storage.StateInterrupted)
			}
		case "H3TY":
			healthy = true
		}
	}
	if !quarantined {
		t.Fatal("the damaged session was dropped; ReconcileHost would then declare live sessions dead")
	}
	if !healthy {
		t.Fatal("the healthy session was not reported")
	}
}

func (s *catalogStoreStub) RetireSessions(_ context.Context, _ storage.HostID, retired []storage.SessionID) (int64, error) {
	s.retired = append(s.retired, retired...)
	s.events = append(s.events, "retire-sessions")
	return int64(len(retired)), nil
}

// Nothing pruned finished sessions before, so an always-on host grew without
// bound: the 1 Hz reconciler re-read and re-upserted every row it had ever
// seen, and past roughly fifteen thousand a reconcile no longer fit in its
// tick. Retention retires finished sessions by age and by count, and takes
// their directories with them, since worker.log is the bulk of the cost.
func TestCatalogRetiresFinishedSessionsByAge(t *testing.T) {
	sessionsDir := t.TempDir()
	now := catalogTestTime.Add(30 * 24 * time.Hour)

	// One finished session well past the retention window, one just inside it,
	// and one still running.
	old := catalogTestMeta("0RDD", worker.StateExited, "boot-a")
	old.CreatedAt = now.Add(-8 * 24 * time.Hour)
	exitedAt := old.CreatedAt.Add(time.Second)
	code := 0
	old.ExitedAt = &exitedAt
	old.ExitCode = &code
	writeCatalogMeta(t, sessionsDir, old.ID, old)

	recent := catalogTestMeta("N3WW", worker.StateExited, "boot-a")
	recent.CreatedAt = now.Add(-time.Hour)
	recentExit := recent.CreatedAt.Add(time.Second)
	recent.ExitedAt = &recentExit
	recent.ExitCode = &code
	writeCatalogMeta(t, sessionsDir, recent.ID, recent)

	running := catalogTestMeta("RVNN", worker.StateRunning, "boot-a")
	running.CreatedAt = now.Add(-90 * 24 * time.Hour) // old, but still alive
	writeCatalogMeta(t, sessionsDir, running.ID, running)

	store := &catalogStoreStub{}
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: sessionsDir,
		Host:        catalogTestHost(catalogTestTime),
		Store:       store,
		Probe:       probeFunc(func(context.Context, string) error { return nil }),
		BootID:      func() string { return "boot-a" },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(store.retired) != 1 || store.retired[0] != "0RDD" {
		t.Fatalf("retired = %v, want just the aged-out session", store.retired)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, "0RDD")); !os.IsNotExist(err) {
		t.Fatalf("the retired session's directory survived: %v", err)
	}
	for _, keep := range []string{"N3WW", "RVNN"} {
		if _, err := os.Stat(filepath.Join(sessionsDir, keep)); err != nil {
			t.Fatalf("session %s was retired but should have been kept: %v", keep, err)
		}
	}
	var observedIDs []string
	for _, got := range store.observed {
		observedIDs = append(observedIDs, string(got.ID))
	}
	sort.Strings(observedIDs)
	if len(observedIDs) != 2 || observedIDs[0] != "N3WW" || observedIDs[1] != "RVNN" {
		t.Fatalf("observed = %v, want the two kept sessions", observedIDs)
	}
}

func TestDetachedSessionIsAliveAndProbed(t *testing.T) {
	t.Parallel()

	// Detached means alive with nobody watching. Mapping it to anything
	// finished would let reconciliation retire a session someone fully
	// intends to come back to.
	meta := catalogTestMeta("7K3D", worker.StateDetached, "boot-a")
	session, probe, err := sessionFromMeta("host-a", "7K3D", meta)
	if err != nil {
		t.Fatalf("sessionFromMeta() error = %v", err)
	}
	if session.State != storage.StateDetached {
		t.Fatalf("state = %q, want detached", session.State)
	}
	if !probe {
		t.Fatal("a detached session must still be probed; its worker is live")
	}
}

func TestDetachedSessionRejectsExitFields(t *testing.T) {
	t.Parallel()

	// The same guard running has: a live session carrying an exit code is
	// torn metadata, not a state to trust.
	meta := catalogTestMeta("7K3D", worker.StateDetached, "boot-a")
	code := 0
	meta.ExitCode = &code
	if _, _, err := sessionFromMeta("host-a", "7K3D", meta); err == nil {
		t.Fatal("a detached session with exit fields was accepted")
	}
}
