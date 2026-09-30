package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// gate blocks a hook until released; the release is registered for cleanup so a
// failing test never leaves an operation blocked.
type gate struct {
	entered, release chan struct{}
	enter, open      sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.done)
	return g
}
func (g *gate) arrive() { g.enter.Do(func() { close(g.entered) }) }
func (g *gate) done()   { g.open.Do(func() { close(g.release) }) }

// setup is a setup worker's wait that finishes when released, or fails like the
// daemon's wait once its context ends.
func (g *gate) setup(ctx context.Context) (int, error) {
	g.arrive()
	select {
	case <-g.release:
		return 0, nil
	case <-ctx.Done():
		return -1, fmt.Errorf("wait for setup: %w", ctx.Err())
	}
}

func receive(t *testing.T, results <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", what)
		return nil
	}
}

func TestUpdateKeepsOldPortUntilRollbackIsImpossible(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	oldPort, newPort := freePort(t), freePort(t)
	app := createServerAppOn(t, f, oldPort)
	replacement := newGate(t)
	workers.beforeStart = func(_ context.Context, _, command string) error {
		if command != "replacement" {
			return nil
		}
		replacement.arrive()
		<-replacement.release
		return errors.New("replacement failed to start")
	}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	updated := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "replacement", Port: newPort, UploadID: upload, Digest: digest})
		updated <- err
	}()
	<-replacement.entered
	release, guardErr := f.origin.GuardService(context.Background(), []int{oldPort}, "")
	if guardErr == nil {
		release()
	}
	replacement.done()
	if err := receive(t, updated, "update"); err == nil {
		t.Fatal("replacement startup failure was hidden")
	}
	if guardErr == nil {
		t.Fatalf("an ordinary service claimed port %d while the update of %s could still roll back onto it", oldPort, app.ID)
	}
	if !workers.alive(t, "app "+app.ID) {
		t.Fatalf("rollback did not restart app %s's previous server", app.ID)
	}
}

func TestStaticToServerUpdateReservesCandidatePort(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createStaticApp(t, f)
	port := freePort(t)
	setup := newGate(t)
	workers.wait = setup.setup
	upload, digest := uploadSource(t, f, sourceFixture(t))
	updated := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "serve", Setup: "npm ci", Port: port, UploadID: upload, Digest: digest})
		updated <- err
	}()
	<-setup.entered
	other, otherDigest := uploadSource(t, f, sourceFixture(t))
	_, createErr := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "server", Command: "serve", Port: port, UploadID: other, Digest: otherDigest})
	setup.done()
	_ = receive(t, updated, "update")
	if createErr == nil {
		t.Fatalf("a new app took port %d while app %s's server update was preparing on it", port, app.ID)
	}
}

func TestSyncRevokesAppDuringBlockedUpdate(t *testing.T) {
	cases := []struct {
		name    string
		deleted bool
		revoke  func(t *testing.T, f *appFixture, id string)
	}{
		{"deleted at edge", true, func(t *testing.T, f *appFixture, id string) { deleteAtEdge(t, f, id) }},
		{"lease lapsed offline", false, func(t *testing.T, f *appFixture, _ string) {
			f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, errors.New("edge offline") }
			f.now = f.now.Add(LeaseTTL)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { testRevokedDuringUpdate(t, tc.deleted, tc.revoke) })
	}
}

func testRevokedDuringUpdate(t *testing.T, deleted bool, revoke func(*testing.T, *appFixture, string)) {
	t.Helper()
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	root, port := f.origin.state.Apps[app.ID].Root, f.origin.state.Apps[app.ID].Port
	setup := newGate(t)
	workers.wait = setup.setup
	upload, digest := uploadSource(t, f, sourceFixture(t))
	updated := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "serve", Setup: "npm ci", Port: port, UploadID: upload, Digest: digest})
		updated <- err
	}()
	<-setup.entered
	revoke(t, f, app.ID)
	syncErr := f.origin.Sync(context.Background())
	if workers.alive(t, "app "+app.ID) || workers.alive(t, "app-setup "+app.ID) {
		t.Fatalf("app %s kept its workers after being revoked during an update: sync error: %v", app.ID, syncErr)
	}
	if err := receive(t, updated, "revoked update"); err == nil {
		t.Fatal("revoked update reported success")
	}
	if workers.alive(t, "app "+app.ID) {
		t.Fatalf("revoked update restarted app %s", app.ID)
	}
	if !deleted {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("lapsed lease deleted app %s's files: %v", app.ID, err)
		}
		return
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireEdgeCleanup(t, f, app.ID)
}

func TestCleanupStopsWorkersOfAppMissingOnOrigin(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	if _, err := workers.Start(context.Background(), "ordinary", "sleep 600", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	delete(f.origin.state.Apps, app.ID)
	f.origin.state.Receipts = map[string]createReceipt{}
	for range 2 {
		if err := f.origin.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	requireEdgeCleanup(t, f, app.ID)
	if workers.alive(t, "app "+app.ID) {
		t.Fatalf("server of app %s, missing on the origin, survived its cleanup", app.ID)
	}
	if !workers.alive(t, "ordinary") {
		t.Fatal("app cleanup stopped an ordinary session")
	}
}

func TestSyncReachesSafetyWhileExchangeIsHeld(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	f.now = f.now.Add(LeaseTTL)
	held := newGate(t)
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) {
		held.arrive()
		<-held.release
		return Signed{}, errors.New("edge unreachable")
	}
	go func() { _, _ = f.origin.Handle(context.Background(), Request{Action: "list"}) }()
	<-held.entered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	synced := make(chan error, 1)
	go func() { synced <- f.origin.Sync(ctx) }()
	select {
	case err := <-synced:
		if err == nil {
			t.Fatal("Sync hid that it never reached the edge")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sync waited for another request's edge exchange past its deadline")
	}
	if !workers.wasStopped(app.ID) {
		t.Fatalf("expired app %s kept running while another request held the edge exchange", app.ID)
	}
}

// recordFaultStore fails the first save whose state carries an app with the
// given edge status, as a store fault while applying a signed sync would.
type recordFaultStore struct {
	*memoryAppStore
	status  string
	tripped bool
}

func (s *recordFaultStore) SaveAppState(ctx context.Context, key string, data []byte) error {
	var state originState
	_ = json.Unmarshal(data, &state)
	for _, a := range state.Apps {
		if key == "apps.origin" && !s.tripped && a.Record.Status == s.status {
			s.tripped = true
			return errors.New("state store unavailable")
		}
	}
	return s.memoryAppStore.SaveAppState(ctx, key, data)
}

func TestSyncActsOnRecordsItCouldNotPersist(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	deleteAtEdge(t, f, app.ID)
	store := &recordFaultStore{memoryAppStore: f.originStore, status: "deleted"}
	f.origin.config.Store = store
	err := f.origin.Sync(context.Background())
	if !store.tripped {
		t.Fatal("fault not triggered")
	}
	if !workers.wasStopped(app.ID) {
		t.Fatalf("deleted app %s kept running because its signed record could not be saved: %v", app.ID, err)
	}
	if err == nil {
		t.Fatal("Sync hid the persistence failure")
	}
}

func TestRecoveryKeepsNewerActivation(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	snapshot := map[string]Record{app.ID: f.origin.state.Apps[app.ID].Record}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	f.origin.config.Store = &appPhaseFaultStore{memoryAppStore: f.originStore, fault: true, phase: "ready"}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, UploadID: upload, Digest: digest}); err == nil {
		t.Fatal("fault not triggered")
	}
	current := f.origin.state.Apps[app.ID]
	if current.Commit == nil || current.Record.Generation != app.Generation+1 {
		t.Fatalf("setup: want an unfinished generation %d commit, have %#v", app.Generation+1, current)
	}
	if err := f.origin.recover(context.Background(), app.ID, snapshot); err != nil {
		t.Fatal(err)
	}
	if got := f.origin.state.Apps[app.ID].Record.Generation; got != app.Generation+1 {
		t.Fatalf("recovery from an earlier sync snapshot reverted app %s to generation %d", app.ID, got)
	}
}
