package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// serverWorkers backs each "app" worker with a real loopback listener, so server
// apps pass the origin's readiness and listener checks. Like the daemon's
// catalog lookup, it refuses a cancelled context.
type serverWorkers struct {
	*fakeWorkers
	mu        sync.Mutex
	listeners map[string]net.Listener
	findErr   map[string]error
}

func newServerWorkers(t *testing.T, f *appFixture) *serverWorkers {
	t.Helper()
	w := &serverWorkers{fakeWorkers: f.workers, listeners: map[string]net.Listener{}, findErr: map[string]error{}}
	t.Cleanup(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, listener := range w.listeners {
			_ = listener.Close()
		}
	})
	f.origin.config.Workers = w
	return w
}

func (w *serverWorkers) Start(ctx context.Context, label, command, root string, env []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("start %s: %w", label, err)
	}
	id, err := w.fakeWorkers.Start(ctx, label, command, root, env)
	if err != nil || !strings.HasPrefix(label, "app ") {
		return id, err
	}
	var port string
	for _, value := range env {
		if value, ok := strings.CutPrefix(value, "PORT="); ok {
			port = value
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		return "", fmt.Errorf("listen for %s: %w", label, err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.listeners[id] = listener
	return id, nil
}

func (w *serverWorkers) Stop(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("stop %s: %w", id, err)
	}
	w.mu.Lock()
	if listener, ok := w.listeners[id]; ok {
		_ = listener.Close()
		delete(w.listeners, id)
	}
	w.mu.Unlock()
	return w.fakeWorkers.Stop(ctx, id)
}

func (w *serverWorkers) Find(ctx context.Context, label string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, fmt.Errorf("find %s: %w", label, err)
	}
	w.mu.Lock()
	err := w.findErr[label]
	w.mu.Unlock()
	if err != nil {
		return "", false, err
	}
	return w.fakeWorkers.Find(ctx, label)
}

func (w *serverWorkers) wasStopped(id string) bool {
	w.fakeWorkers.mu.Lock()
	defer w.fakeWorkers.mu.Unlock()
	return slices.Contains(w.stopped, "worker-app "+id)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

func createServerApp(t *testing.T, f *appFixture) Record {
	t.Helper()
	upload, digest := uploadSource(t, f, sourceFixture(t))
	result, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "server", Command: "serve", Port: freePort(t), UploadID: upload, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	return *result.App
}

// sortedServerApps returns two server apps in the edge's listing order.
func sortedServerApps(t *testing.T, f *appFixture) (Record, Record) {
	t.Helper()
	apps := []Record{createServerApp(t, f), createServerApp(t, f)}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	return apps[0], apps[1]
}

func serveStatus(t *testing.T, f *appFixture, app Record) int {
	t.Helper()
	proof, err := Sign("mesh-app/admission/v1", identityFor(f.ownerKey), 1, admission{ID: app.ID, Generation: app.Generation, Method: http.MethodGet, URI: "/", Host: app.ID + "." + Domain, Until: f.now.Add(time.Minute)}, f.edgeKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(proof)
	request := httptest.NewRequest(http.MethodGet, "http://origin/.mesh-app/origin/"+app.ID+"/", nil)
	request.Header.Set("X-Mesh-App-Admission", base64.RawURLEncoding.EncodeToString(raw))
	response := httptest.NewRecorder()
	f.origin.ServeHTTP(response, request)
	return response.Code
}

func deleteAtEdge(t *testing.T, f *appFixture, id string) {
	t.Helper()
	f.edge.mu.Lock()
	defer f.edge.mu.Unlock()
	if _, err := f.edge.apply(context.Background(), identityFor(f.ownerKey), Request{Action: "delete", ID: id}); err != nil {
		t.Fatal(err)
	}
}

func requireEdgeCleanup(t *testing.T, f *appFixture, id string) {
	t.Helper()
	record, _, err := f.edge.lookup(context.Background(), id, false)
	if err != nil || record.Cleanup != "complete" {
		t.Fatalf("edge cleanup of app %s not confirmed: %#v %v", id, record, err)
	}
}

// startBlockedSetup begins creating a static app whose setup waits until the
// returned release runs, and returns once that setup has started.
func startBlockedSetup(t *testing.T, f *appFixture) (release func() error) {
	t.Helper()
	workers := &reviewBlockingWorkers{fakeWorkers: f.workers, entered: make(chan struct{}), release: make(chan struct{})}
	f.origin.config.Workers = workers
	upload, digest := uploadSource(t, f, sourceFixture(t))
	created := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "create", Setup: "npm ci", UploadID: upload, Digest: digest})
		created <- err
	}()
	<-workers.entered
	var once sync.Once
	return func() error {
		once.Do(func() { close(workers.release) })
		return <-created
	}
}

func TestLeaseStopAfterExchangeTimeout(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	f.now = f.now.Add(LeaseTTL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.origin.config.Exchange = func(exchange context.Context, _ Signed) (Signed, error) {
		// The edge stays silent until the maintenance budget runs out.
		cancel()
		<-exchange.Done()
		return Signed{}, exchange.Err()
	}
	err := f.origin.Sync(ctx)
	if !workers.wasStopped(app.ID) {
		t.Fatalf("expired app %s kept running after the lease exchange timed out: stopped=%v, sync error: %v", app.ID, workers.stopped, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync hid the exchange failure: %v", err)
	}
}

func TestSyncRenewsLeasesPastAFailingApp(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	first, later := sortedServerApps(t, f)
	workers.findErr["app "+first.ID] = errors.New("worker catalog unavailable")
	f.now = f.now.Add(LeaseTTL + time.Second)
	err := f.origin.Sync(context.Background())
	route := (*f.origin.routes.Load())[later.ID]
	if !f.now.Before(route.Record.LeaseUntil) {
		t.Fatalf("app %s lost its lease renewal behind failing app %s: lease until %s, now %s, sync error: %v", later.ID, first.ID, route.Record.LeaseUntil, f.now, err)
	}
	if err == nil || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("Sync hid or misattributed app %s's failure: %v", first.ID, err)
	}
}

func TestSyncCleansUpPastAFailingApp(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	first, later := sortedServerApps(t, f)
	laterRoot := f.origin.state.Apps[later.ID].Root
	workers.findErr["app "+first.ID] = errors.New("worker catalog unavailable")
	deleteAtEdge(t, f, later.ID)
	err := f.origin.Sync(context.Background())
	if !workers.wasStopped(later.ID) {
		t.Fatalf("deleted app %s kept running behind failing app %s: sync error: %v", later.ID, first.ID, err)
	}
	if _, statErr := os.Stat(laterRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("deleted app %s kept its files behind failing app %s: %v", later.ID, first.ID, statErr)
	}
	requireEdgeCleanup(t, f, later.ID)
	if err == nil || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("Sync hid or misattributed app %s's failure: %v", first.ID, err)
	}
}

func TestLeaseRenewalContinuesDuringSlowSetup(t *testing.T) {
	f := newAppFixture(t)
	first := createStaticApp(t, f)
	finish := startBlockedSetup(t, f)
	f.now = f.now.Add(LeaseTTL - time.Second)
	synced := make(chan error, 1)
	go func() { synced <- f.origin.Sync(context.Background()) }()
	select {
	case err := <-synced:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = finish()
		t.Fatal("Sync waited behind another app's setup")
	}
	f.now = f.now.Add(2 * time.Second)
	status := serveStatus(t, f, first)
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("app %s became unavailable while another app ran setup: status %d", first.ID, status)
	}
}

func TestServiceGuardDoesNotWaitForSetup(t *testing.T) {
	f := newAppFixture(t)
	finish := startBlockedSetup(t, f)
	guarded := make(chan error, 1)
	go func() {
		release, err := f.origin.GuardService(context.Background(), []int{31999}, "")
		if err == nil {
			release()
		}
		guarded <- err
	}()
	select {
	case err := <-guarded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = finish()
		t.Fatal("ordinary service registration waited behind an app's setup")
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
}

func TestSyncStopsThenRemovesAppTheEdgeNoLongerLists(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	root := f.origin.state.Apps[app.ID].Root
	lease := f.origin.state.Apps[app.ID].Record.LeaseUntil
	f.edge.mu.Lock()
	delete(f.edge.state.Apps, app.ID)
	f.edge.mu.Unlock()
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if workers.wasStopped(app.ID) {
		t.Fatal("stopped an unlisted app before its last lease lapsed")
	}
	f.now = lease
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !workers.wasStopped(app.ID) {
		t.Fatalf("app %s kept running after the edge stopped listing it and its lease lapsed", app.ID)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("absence from the edge list deleted app %s's files before the grace period: %v", app.ID, err)
	}
	f.now = lease.Add(IdleTTL)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.origin.state.Apps[app.ID]; ok {
		t.Fatalf("app %s the edge no longer lists was kept past the grace period", app.ID)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("app %s's files remain past the grace period: %v", app.ID, err)
	}
}

func TestSyncDeletesEdgeAppMissingOnOrigin(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	// The origin lost this app, for example to a state restore, while the edge
	// still routes its name here and every request would extend its deadline.
	delete(f.origin.state.Apps, app.ID)
	f.origin.state.Receipts = map[string]createReceipt{}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || record.Status != "deleted" {
		t.Fatalf("edge keeps routing app %s that its origin no longer has: %#v %v", app.ID, record, err)
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireEdgeCleanup(t, f, app.ID)
}
