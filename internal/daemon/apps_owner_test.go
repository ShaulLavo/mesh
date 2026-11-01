package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

const testAppOwner = "app test\n0123456789abcdef0123456789abcdef"

func ownerAdapter(root string) appWorkers {
	return appWorkers{lifecycle: &lifecycle{sessionsDir: root, context: context.Background(), operationTimeout: 5 * time.Second}}
}
func writeTestOwner(t *testing.T, dir, marker string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Owner(dir), []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestAppWorkersOwnerMarker(t *testing.T) {
	t.Run("written-at-reservation", testAppOwnerReservation)
	t.Run("release-matching-only", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "7K3D")
		writeTestOwner(t, dir, testAppOwner)
		w := ownerAdapter(root)
		if err := w.Release(context.Background(), "7K3D", "app test\nffffffffffffffffffffffffffffffff"); err != nil {
			t.Fatal(err)
		}
		if got, err := w.Owner(context.Background(), "7K3D"); err != nil || got != testAppOwner {
			t.Fatalf("mismatch unpinned: %q %v", got, err)
		}
		if err := w.Release(context.Background(), "7K3D", testAppOwner); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Owner(context.Background(), "7K3D"); !errors.Is(err, apps.ErrNoOwner) {
			t.Fatal(err)
		}
	})
	t.Run("release-missing", func(t *testing.T) {
		root := t.TempDir()
		w := ownerAdapter(root)
		if err := w.Release(context.Background(), "7K3D", testAppOwner); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "7K3D"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := w.Release(context.Background(), "7K3D", testAppOwner); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("owner-missing-directory", func(t *testing.T) {
		if _, err := ownerAdapter(t.TempDir()).Owner(context.Background(), "7K3D"); !errors.Is(err, apps.ErrNoOwner) {
			t.Fatal(err)
		}
	})
	for _, phase := range []string{"unlink", "sync", "missing-marker-sync", "missing-directory-sync"} {
		t.Run(phase, func(t *testing.T) { testAppOwnerReleaseFault(t, phase) })
	}
}
func testAppOwnerReservation(t *testing.T) {
	sessions := compactSocketTempDir(t)
	catalog := &lifecycleTestCatalog{}
	connector, err := newWorkerConnector(sessions, catalog)
	if err != nil {
		t.Fatal(err)
	}
	seen := ""
	l := mustLifecycle(t, lifecycleConfig{Catalog: catalog, Connector: connector, Host: storage.Host{ID: "host-a", MeshIdentity: "key", LastSeenAt: time.Now()}, SessionsDir: sessions, Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
		label, token, err := apps.ParseWorkerOwner(cfg.Owner)
		if err != nil || label != cfg.Label || len(token) != 32 {
			return worker.Launched{}, errors.New("launch lost full owner")
		}
		seen = cfg.Owner
		writeTestOwner(t, filepath.Join(sessions, "7K3D"), cfg.Owner)
		return worker.Launched{}, &worker.StartedError{ID: "7K3D", Err: errors.New("after start")}
	}})
	w := appWorkers{lifecycle: l}
	if id, err := w.Start(context.Background(), "app test", "setup", sessions, nil); id != "7K3D" || err == nil {
		t.Fatalf("start %q %v", id, err)
	}
	if got, err := w.Owner(context.Background(), "7K3D"); err != nil || got != seen {
		t.Fatalf("owner %q %v", got, err)
	}
}
func testAppOwnerReleaseFault(t *testing.T, phase string) {
	root := t.TempDir()
	dir := filepath.Join(root, "7K3D")
	w := ownerAdapter(root)
	if phase != "missing-directory-sync" {
		writeTestOwner(t, dir, testAppOwner)
	}
	if phase == "missing-marker-sync" {
		if err := os.Remove(paths.Owner(dir)); err != nil {
			t.Fatal(err)
		}
	}
	fault := errors.New("injected owner I/O")
	unlink := func(root *os.Root, name string) error {
		if phase == "unlink" {
			return fault
		}
		return root.Remove(name)
	}
	syncDir := func(*os.Root) error { return fault }
	if err := w.releaseOwner(context.Background(), "7K3D", testAppOwner, unlink, syncDir); !errors.Is(err, fault) {
		t.Fatalf("release fault %v", err)
	}
	if err := w.Release(context.Background(), "7K3D", testAppOwner); err != nil {
		t.Fatal(err)
	}
	if phase == "missing-directory-sync" {
		return
	}
	replacement := "app test\nffffffffffffffffffffffffffffffff"
	writeTestOwner(t, dir, replacement)
	if err := w.Release(context.Background(), "7K3D", testAppOwner); err != nil {
		t.Fatal(err)
	}
	if got, err := w.Owner(context.Background(), "7K3D"); err != nil || got != replacement {
		t.Fatalf("retry unpinned replacement %q %v", got, err)
	}
}

func TestCatalogKeepsOwnedSessions(t *testing.T) {
	for _, mode := range []string{"age", "count", "forgotten", "remove", "malformed", "unreadable"} {
		t.Run(mode, func(t *testing.T) { catalogOwnedSession(t, mode) })
	}
}
func catalogOwnedSession(t *testing.T, mode string) {
	root := t.TempDir()
	id := storage.SessionID("7K3D")
	dir := filepath.Join(root, string(id))
	meta := catalogTestMeta(string(id), worker.StateExited, "boot-a")
	code, exited := 0, catalogTestTime
	meta.ExitCode, meta.ExitedAt = &code, &exited
	if mode != "count" {
		meta.CreatedAt = catalogTestTime.Add(-8 * 24 * time.Hour)
	}
	writeCatalogMeta(t, root, string(id), meta)
	marker := testAppOwner
	if mode == "malformed" {
		marker = "not a marker"
	}
	writeTestOwner(t, dir, marker)
	if mode == "unreadable" {
		if err := os.Remove(paths.Owner(dir)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(paths.Owner(dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "forgotten" {
		if err := os.WriteFile(paths.Forgotten(dir), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := &catalogStoreStub{}
	c := newCatalogForTest(t, root, store, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.retired) != 0 || len(store.observed) != 1 {
		t.Fatalf("pinned row retired: %v %+v", store.retired, store.observed)
	}
	if err := c.Remove(context.Background(), id); err == nil {
		t.Fatal("online removal discarded pin")
	}
	if err := removeUnclaimedRetiredSession(dir); err == nil {
		t.Fatal("forgotten fast path discarded pin")
	}
	if err := removeRetiredSession(dir); err == nil {
		t.Fatal("physical deletion discarded pin")
	}
	if mode == "count" {
		checkOwnedCountRetention(t, c, id, true)
	}
	if mode == "unreadable" {
		if err := os.Remove(paths.Owner(dir)); err != nil {
			t.Fatal(err)
		}
	} else if err := ownerAdapter(root).Release(context.Background(), string(id), marker); err != nil {
		t.Fatal(err)
	}
	if mode == "count" {
		checkOwnedCountRetention(t, c, id, false)
	}
	if err := c.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released directory retained: %v", err)
	}
}
func checkOwnedCountRetention(t *testing.T, c *Catalog, id storage.SessionID, pinned bool) {
	t.Helper()
	rows := []storage.Session{{ID: id, State: storage.StateExited, CreatedAt: catalogTestTime.Add(-time.Hour)}}
	for i := range maxRetainedTerminalSessions {
		rows = append(rows, storage.Session{ID: storage.SessionID(fmt.Sprintf("row-%d", i)), State: storage.StateExited, CreatedAt: catalogTestTime})
	}
	_, retired := c.retireFinishedSessions(rows)
	if slices.Contains(retired, id) == pinned {
		t.Fatalf("count retention pinned=%t retired=%v", pinned, retired)
	}
}

// This fixture exercises the actual adapter, connector, retirement and socket
// lifetime while an origin-equivalent operation keeps its reservation held.
func TestAppOwnerSocketLifetime(t *testing.T) {
	root := compactSocketTempDir(t)
	fixture := newAppOwnershipSocketWorker(t, root, "app test")
	writeTestOwner(t, fixture.dir, testAppOwner)
	w := ownerAdapter(root)
	marker, err := w.Owner(context.Background(), fixture.id)
	if err != nil {
		t.Fatal(err)
	}
	checked, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(proceed) }) })
	done := make(chan error, 1)
	go func() { close(checked); <-proceed; done <- w.Stop(context.Background(), fixture.id) }()
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("owner barrier")
	}
	if err := removeRetiredSession(fixture.dir); err == nil {
		t.Fatal("retirement crossed reservation")
	}
	if err := os.Mkdir(fixture.dir, 0700); !errors.Is(err, os.ErrExist) {
		t.Fatalf("reserved ID reused: %v", err)
	}
	once.Do(func() { close(proceed) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not join")
	}
	if err := w.Release(context.Background(), fixture.id, marker); err != nil {
		t.Fatal(err)
	}
}

type originBoundaryWorkers struct {
	appWorkers
	entered  chan struct{}
	proceed  chan struct{}
	releases chan string
	store    *storage.Store
}

func (w *originBoundaryWorkers) Stop(ctx context.Context, id string) error {
	close(w.entered)
	<-w.proceed
	return w.appWorkers.Stop(ctx, id)
}
func (w *originBoundaryWorkers) Release(ctx context.Context, id, marker string) error {
	data, err := w.store.LoadAppState(ctx, "apps.origin")
	if err != nil {
		return fmt.Errorf("read durable completion: %w", err)
	}
	var state struct {
		Launched map[string][]struct {
			Token string
			Ended bool
		}
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode durable completion: %w", err)
	}
	claims := state.Launched["7k3d"]
	if len(claims) != 1 || !claims[0].Ended || "app 7k3d\n"+claims[0].Token != marker {
		return errors.New("release preceded complete durable end")
	}
	w.releases <- marker
	return w.appWorkers.Release(ctx, id, marker)
}
func TestAppOwnerOriginBarrier(t *testing.T) {
	sessions := compactSocketTempDir(t)
	fixture := newAppOwnershipSocketWorker(t, sessions, "app 7k3d")
	marker := "app 7k3d\n0123456789abcdef0123456789abcdef"
	writeTestOwner(t, fixture.dir, marker)
	store, _, _ := newServiceControllerTest(t, "/mesh")
	root := t.TempDir()
	state := map[string]any{
		"apps":      map[string]any{"7k3d": map[string]any{"record": apps.Record{ID: "7k3d", Kind: "static", Status: "active", LeaseUntil: time.Now().Add(-time.Hour)}, "root": root, "phase": "ready"}},
		"launched":  map[string]any{"7k3d": []map[string]any{{"id": fixture.id, "label": "app 7k3d", "token": "0123456789abcdef0123456789abcdef", "gen": 1}}},
		"launchSeq": 1,
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAppState(context.Background(), "apps.origin", data); err != nil {
		t.Fatal(err)
	}
	_, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &originBoundaryWorkers{appWorkers: ownerAdapter(sessions), entered: make(chan struct{}), proceed: make(chan struct{}), releases: make(chan string, 2), store: store}
	w.lifecycle.catalog = &lifecycleTestCatalog{}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(w.proceed) }) })
	origin, err := apps.NewOrigin(context.Background(), apps.OriginConfig{Store: store, Key: key, Workers: w, DataRoot: root, Exchange: func(context.Context, apps.Signed) (apps.Signed, error) {
		return apps.Signed{}, errors.New("registry offline")
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(origin.Close)
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() { defer close(joined); done <- origin.Sync(context.Background()) }()
	t.Cleanup(func() {
		once.Do(func() { close(w.proceed) })
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("origin boundary did not join during cleanup")
		}
	})
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("origin never reached owned stop")
	}
	assertPinnedOriginBoundary(t, sessions, fixture.dir, fixture.id, w.releases)
	once.Do(func() { close(w.proceed) })
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("offline registry error lost")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("origin stop did not join")
	}
	select {
	case got := <-w.releases:
		if got != marker {
			t.Fatal("release changed identity")
		}
	default:
		t.Fatal("origin failed to release completed reservation")
	}
	if _, err := w.Owner(context.Background(), fixture.id); !errors.Is(err, apps.ErrNoOwner) {
		t.Fatalf("reservation remains: %v", err)
	}
}
func assertPinnedOriginBoundary(t *testing.T, root, dir, id string, releases <-chan string) {
	t.Helper()
	select {
	case <-releases:
		t.Fatal("release overtook outstanding stop")
	default:
	}
	c := newCatalogForTest(t, root, &catalogStoreStub{}, probeFunc(func(context.Context, string) error { return nil }), func() string { return "boot-a" })
	if err := c.Remove(context.Background(), storage.SessionID(id)); err == nil {
		t.Fatal("catalog removed owner during stop")
	}
	if err := removeRetiredSession(dir); err == nil {
		t.Fatal("physical retirement overtook stop")
	}
	if err := os.Mkdir(dir, 0700); !errors.Is(err, os.ErrExist) {
		t.Fatalf("ID reused during owned action: %v", err)
	}
}
