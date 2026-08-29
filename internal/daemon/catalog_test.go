package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	dead := errors.New("connection refused")
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
			meta:        catalogTestMeta("LIVE", worker.StateRunning, "boot-a"),
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
			meta:        catalogTestMeta("REBOOT", worker.StateRunning, "boot-a"),
			currentBoot: "boot-b",
			wantState:   storage.StateInterrupted,
			wantProbes:  0,
		},
		{
			name: "exited worker",
			meta: func() worker.Meta {
				meta := catalogTestMeta("EXITED", worker.StateExited, "boot-a")
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
			meta:        catalogTestMeta("NOBID", worker.StateRunning, ""),
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
			if got := store.events; !reflect.DeepEqual(got, []string{"upsert", "reconcile"}) {
				t.Fatalf("store call order = %v, want [upsert reconcile]", got)
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
			name: "missing metadata",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, "MISSING"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed metadata",
			setup: func(t *testing.T, root string) {
				t.Helper()
				dir := filepath.Join(root, "BROKEN")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
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
				writeCatalogMeta(t, root, "UNKNOWN", catalogTestMeta("UNKNOWN", "detached", "boot-a"))
			},
		},
		{
			name: "worker-derived interrupted state",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, "INTR", catalogTestMeta("INTR", worker.StateInterrupted, "boot-a"))
			},
		},
		{
			name: "empty command",
			setup: func(t *testing.T, root string) {
				t.Helper()
				meta := catalogTestMeta("NOCMD", worker.StateRunning, "boot-a")
				meta.Command = nil
				writeCatalogMeta(t, root, meta.ID, meta)
			},
		},
		{
			name: "zero PID",
			setup: func(t *testing.T, root string) {
				t.Helper()
				meta := catalogTestMeta("NOPID", worker.StateRunning, "boot-a")
				meta.PID = 0
				writeCatalogMeta(t, root, meta.ID, meta)
			},
		},
		{
			name: "zero creation time",
			setup: func(t *testing.T, root string) {
				t.Helper()
				meta := catalogTestMeta("NOTIME", worker.StateRunning, "boot-a")
				meta.CreatedAt = time.Time{}
				writeCatalogMeta(t, root, meta.ID, meta)
			},
		},
		{
			name: "running worker with exit fields",
			setup: func(t *testing.T, root string) {
				t.Helper()
				meta := catalogTestMeta("RUNEXIT", worker.StateRunning, "boot-a")
				meta.ExitedAt = &exitedAt
				meta.ExitCode = &exitCode
				writeCatalogMeta(t, root, meta.ID, meta)
			},
		},
		{
			name: "exited worker without exit fields",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeCatalogMeta(t, root, "NOEXIT", catalogTestMeta("NOEXIT", worker.StateExited, "boot-a"))
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
	writeCatalogMeta(t, sessionsDir, "ZED", catalogTestMeta("ZED", worker.StateRunning, "boot-a"))
	writeCatalogMeta(t, sessionsDir, "ALPHA", catalogTestMeta("ALPHA", worker.StateRunning, "boot-a"))
	writeCatalogMeta(t, sessionsDir, "MIDDLE", catalogTestMeta("MIDDLE", worker.StateRunning, "boot-a"))

	store := &catalogStoreStub{}
	catalog := newCatalogForTest(t, sessionsDir, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}

	got := make([]storage.SessionID, len(store.observed))
	for i := range store.observed {
		got[i] = store.observed[i].ID
	}
	want := []storage.SessionID{"ALPHA", "MIDDLE", "ZED"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observation order = %v, want %v", got, want)
	}
}

func TestCatalogReconcileCancellationDuringProbeDoesNotWrite(t *testing.T) {
	sessionsDir := t.TempDir()
	writeCatalogMeta(t, sessionsDir, "CANCEL", catalogTestMeta("CANCEL", worker.StateRunning, "boot-a"))

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

	liveMeta := catalogTestMeta("LIVE", worker.StateRunning, "boot-a")
	missingMeta := catalogTestMeta("MISSING", worker.StateRunning, "boot-a")
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

func TestCatalogBrokenScanPreservesSQLiteState(t *testing.T) {
	ctx := context.Background()
	sessionsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(sessionsDir, "BROKEN"), 0o700); err != nil {
		t.Fatal(err)
	}
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

	if _, err := catalog.List(nil); err == nil {
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
	observed       []storage.Session
}

func (s *catalogStoreStub) UpsertHost(_ context.Context, host storage.Host) (storage.Host, error) {
	s.upsertCalls++
	s.events = append(s.events, "upsert")
	return host, nil
}

func (s *catalogStoreStub) ReconcileHost(_ context.Context, _ storage.HostID, observed []storage.Session) error {
	s.reconcileCalls++
	s.events = append(s.events, "reconcile")
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
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
	_, err := catalog.Get(context.Background(), storage.SessionID(strings.Repeat("x", 9)))
	if err == nil {
		t.Fatal("Get(overlong ID) error = nil")
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
