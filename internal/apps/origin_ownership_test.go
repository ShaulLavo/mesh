package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
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

func (g *gate) waitFor(results <-chan error) error {
	select {
	case <-g.entered:
		return nil
	case err := <-results:
		return errors.Join(errors.New("operation finished before reaching hook"), err)
	}
}

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
	if err := replacement.waitFor(updated); err != nil {
		t.Fatal(err)
	}
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

func TestUpdatePortCollisionFinishesBeforeReplacementHook(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	port := freePort(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	replacement := newGate(t)
	workers.beforeStart = func(_ context.Context, _, command string) error {
		if command == "replacement" {
			replacement.arrive()
		}
		return nil
	}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	updated := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "replacement", Port: port, UploadID: upload, Digest: digest})
		updated <- err
	}()
	waited := make(chan error, 1)
	go func() { waited <- replacement.waitFor(updated) }()
	err = receive(t, waited, "hook wait after port collision")
	if err == nil || !strings.Contains(err.Error(), "server port already has a listener") {
		t.Fatalf("hook wait hid the pre-start port collision: %v", err)
	}
	if !workers.alive(t, "app "+app.ID) {
		t.Fatal("port collision did not restore the old server")
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

func TestSyncLetsOwnersDeleteFinish(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	cleaning := newGate(t)
	var once sync.Once
	hook := func(ctx context.Context, label string) error {
		if label != "app "+app.ID {
			return nil
		}
		var err error
		once.Do(func() {
			cleaning.arrive()
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				err = fmt.Errorf("find %s: %w", label, ctx.Err())
			}
		})
		return err
	}
	workers.mu.Lock()
	workers.beforeFind = hook
	workers.mu.Unlock()
	deleted := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "delete", ID: app.ID})
		deleted <- err
	}()
	<-cleaning.entered
	syncErr := f.origin.Sync(context.Background())
	if err := receive(t, deleted, "owner's delete"); err != nil {
		t.Fatalf("Sync broke the owner's delete of %s it was already carrying out: %v (sync: %v)", app.ID, err, syncErr)
	}
	requireEdgeCleanup(t, f, app.ID)
	if workers.alive(t, "app "+app.ID) {
		t.Fatalf("deleted app %s kept running", app.ID)
	}
}

func TestCreateCannotTakePortAnUpdateReserved(t *testing.T) {
	f := newAppFixture(t)
	newServerWorkers(t, f)
	updating := createStaticApp(t, f)
	port := freePort(t)
	allocating := newGate(t)
	exchange := f.origin.config.Exchange
	f.origin.config.Exchange = func(ctx context.Context, s Signed) (Signed, error) {
		var q Request
		_ = json.Unmarshal(s.Body, &q)
		if q.Action == "allocate" {
			allocating.arrive()
			<-allocating.release
		}
		return exchange(ctx, s)
	}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	created := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "server", Command: "serve", Port: port, UploadID: upload, Digest: digest})
		created <- err
	}()
	<-allocating.entered
	next, nextDigest := uploadSource(t, f, sourceFixture(t))
	updated := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: updating.ID, Kind: "server", Command: "serve", Port: port, UploadID: next, Digest: nextDigest})
		updated <- err
	}()
	waitFor(t, func() bool {
		f.origin.mu.Lock()
		defer f.origin.mu.Unlock()
		op := f.origin.ops["app "+updating.ID]
		return op != nil && slices.Contains(op.ports, port)
	}, "update reserving its candidate port")
	allocating.done()
	createErr := receive(t, created, "create")
	_ = receive(t, updated, "update")
	if createErr == nil {
		t.Fatalf("a create took port %d that app %s's update had reserved while the create was allocating", port, updating.ID)
	}
}

func TestUpdateWithoutAppIDIsRejected(t *testing.T) {
	f := newAppFixture(t)
	newServerWorkers(t, f)
	upload, digest := uploadSource(t, f, sourceFixture(t))
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("update without an app ID panicked: %v", p)
			}
		}()
		_, err = f.origin.Handle(context.Background(), Request{Action: "update", Kind: "server", Command: "serve", Port: freePort(t), UploadID: upload, Digest: digest})
	}()
	if err == nil {
		t.Fatal("update without an app ID was accepted")
	}
}

func TestSyncEnforcesLeaseThatElapsedInTransit(t *testing.T) {
	for _, running := range []bool{true, false} {
		name := "running"
		if !running {
			name = "stopped"
		}
		t.Run(name, func(t *testing.T) { testLeaseElapsedInTransit(t, running) })
	}
}

func testLeaseElapsedInTransit(t *testing.T, running bool) {
	t.Helper()
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	root := f.origin.state.Apps[app.ID].Root
	if !running {
		if err := workers.Stop(context.Background(), "worker-app "+app.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The edge signs a lease that ends at the app's deadline, one second away,
	// and the answer arrives two seconds later.
	f.now = app.ExpiresAt.Add(-time.Second)
	f.origin.config.Exchange = func(ctx context.Context, s Signed) (Signed, error) {
		response, err := f.edge.Exchange(ctx, s)
		f.now = f.now.Add(2 * time.Second)
		return response, err
	}
	err := f.origin.Sync(context.Background())
	if workers.alive(t, "app "+app.ID) {
		t.Fatalf("app %s runs on a lease that had elapsed when Sync applied it (running before: %t): %v", app.ID, running, err)
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("an elapsed lease deleted app %s's files: %v", app.ID, statErr)
	}
}

// ackFaultStore fails the second save after it is armed: the one that clears
// Pending once the edge's signed reply has been verified.
type ackFaultStore struct {
	*memoryAppStore
	saves   int
	tripped bool
}

func (s *ackFaultStore) SaveAppState(ctx context.Context, key string, data []byte) error {
	if key == "apps.origin" {
		s.saves++
		if s.saves == 2 {
			s.tripped = true
			return errors.New("state store unavailable")
		}
	}
	return s.memoryAppStore.SaveAppState(ctx, key, data)
}

func TestSyncActsOnVerifiedReplyItCouldNotAcknowledge(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	deleteAtEdge(t, f, app.ID)
	store := &ackFaultStore{memoryAppStore: f.originStore}
	f.origin.config.Store = store
	err := f.origin.Sync(context.Background())
	if !store.tripped {
		t.Fatal("fault not triggered")
	}
	if !workers.wasStopped(app.ID) {
		t.Fatalf("deleted app %s kept running because the edge's verified reply could not be acknowledged: %v", app.ID, err)
	}
	if err == nil {
		t.Fatal("Sync hid the acknowledgement failure")
	}
}

func TestSafetyStopsSetupWhenServerStopFails(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	port := f.origin.state.Apps[app.ID].Port
	setup := newGate(t)
	workers.wait = setup.setup
	upload, digest := uploadSource(t, f, sourceFixture(t))
	updated := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "serve", Setup: "npm ci", Port: port, UploadID: upload, Digest: digest})
		updated <- err
	}()
	<-setup.entered
	workers.mu.Lock()
	workers.stopErr["worker-app "+app.ID] = errors.New("worker socket unavailable")
	workers.mu.Unlock()
	deleteAtEdge(t, f, app.ID)
	syncErr := f.origin.Sync(context.Background())
	_ = receive(t, updated, "revoked update")
	if workers.alive(t, "app-setup "+app.ID) {
		t.Fatalf("app %s's setup kept running because stopping its server failed: %v", app.ID, syncErr)
	}
	if syncErr == nil {
		t.Fatal("Sync hid the failed server stop")
	}
}

func TestRetiredUploadIsNotRevivedByItsWriter(t *testing.T) {
	f := newAppFixture(t)
	created, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin"})
	if err != nil {
		t.Fatal(err)
	}
	f.origin.mu.Lock()
	cached := f.origin.state.Uploads[created.UploadID]
	// Activation consumes the upload while the writer holds its cached copy.
	delete(f.origin.state.Uploads, created.UploadID)
	f.origin.mu.Unlock()
	cached.Size = 5
	if err := f.origin.recordChunk(context.Background(), cached); err == nil {
		t.Fatal("writer's save of a retired upload succeeded")
	}
	if _, ok := f.origin.state.Uploads[created.UploadID]; ok {
		t.Fatalf("writer revived retired upload %s", created.UploadID)
	}
}

func waitFor(t *testing.T, ready func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
