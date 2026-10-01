package apps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var errSetupCrash = errors.New("daemon crashed during setup")

// crashingSetupWorkers loses an operation in setup the way a daemon crash does:
// the setup worker keeps running and nothing after the wait runs.
type crashingSetupWorkers struct{ *fakeWorkers }

func (w *crashingSetupWorkers) Wait(context.Context, string) (int, error) { panic(errSetupCrash) }

// crashInSetup runs q until its setup starts, abandons it there, and reopens
// the origin from durable state alone.
func crashInSetup(t *testing.T, f *appFixture, q Request) {
	t.Helper()
	f.origin.config.Workers = &crashingSetupWorkers{fakeWorkers: f.workers}
	func() {
		defer func() {
			r := recover()
			if err, ok := r.(error); !ok || !errors.Is(err, errSetupCrash) {
				panic(r)
			}
		}()
		_, err := f.origin.Handle(context.Background(), q)
		t.Fatalf("setup never started: %v", err)
	}()
	restartAppOrigin(t, f)
}

func updatedSourceFixture(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "index.html"), []byte("updated page"), 0600); err != nil {
		t.Fatal(err)
	}
	return source
}

func stagedSources(t *testing.T, f *appFixture) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	var staged []string
	for _, entry := range entries {
		if entry.IsDir() {
			staged = append(staged, entry.Name())
		}
	}
	return staged
}

func TestUpdateCrashTracksSetupAndCandidate(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	oldRoot := f.origin.state.Apps[app.ID].Root
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	crashInSetup(t, f, Request{Action: "update", ID: app.ID, Kind: "static", Setup: "npm ci", UploadID: upload, Digest: digest})
	candidate := filepath.Join(f.root, "apps", app.ID, "source-"+upload)
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("crash fixture left no candidate workspace: %v", err)
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, alive, _ := f.workers.Find(context.Background(), "app-setup "+app.ID); alive {
		t.Fatal("recovery left the interrupted setup worker running")
	}
	if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery left the candidate workspace: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(oldRoot, "index.html")); err != nil || string(data) != "original page" { //nolint:gosec // Reads the fixture's previous revision.
		t.Fatalf("recovery damaged the previous revision: %q %v", data, err)
	}
	if status := serveStatus(t, f, app); status != 200 {
		t.Fatalf("previous revision returned %d after recovery", status)
	}
	record, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || record.Revision != app.Revision || record.Generation != app.Generation {
		t.Fatalf("recovery changed the served revision: %#v %v", record, err)
	}
}

func TestInterruptedUpdateRetryRunsCleanly(t *testing.T) {
	for _, synced := range []bool{false, true} {
		name := "before-recovery"
		if synced {
			name = "after-recovery"
		}
		t.Run(name, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			oldRoot := f.origin.state.Apps[app.ID].Root
			upload, digest := uploadSource(t, f, updatedSourceFixture(t))
			q := Request{Action: "update", ID: app.ID, Kind: "static", Setup: "npm ci", UploadID: upload, Digest: digest}
			crashInSetup(t, f, q)
			if synced {
				if err := f.origin.Sync(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.origin.Handle(context.Background(), q)
			if err != nil {
				t.Fatalf("retry of the interrupted update failed: %v", err)
			}
			if result.App.Revision != upload || result.App.Generation != app.Generation+1 {
				t.Fatalf("retry did not activate the update: %#v", result.App)
			}
			root := f.origin.state.Apps[app.ID].Root
			if data, err := os.ReadFile(filepath.Join(root, "index.html")); err != nil || string(data) != "updated page" { //nolint:gosec // Reads the fixture's new revision.
				t.Fatalf("retry serves the wrong files: %q %v", data, err)
			}
			if _, err := os.Stat(oldRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retry kept the previous workspace: %v", err)
			}
			if staged := stagedSources(t, f); len(staged) != 0 {
				t.Fatalf("retry left staging directories: %v", staged)
			}
		})
	}
}

func TestInterruptedUpdateRetryRefusesChangedRecipe(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	q := Request{Action: "update", ID: app.ID, Kind: "static", Setup: "npm ci", UploadID: upload, Digest: digest}
	crashInSetup(t, f, q)
	q.Setup = "npm install"
	_, err := f.origin.Handle(context.Background(), q)
	if err == nil || !strings.Contains(err.Error(), "conflicts") || !strings.Contains(err.Error(), app.ID) || !strings.Contains(err.Error(), upload) {
		t.Fatalf("changed recipe for an interrupted update was not refused by name: %v", err)
	}
	if status := serveStatus(t, f, app); status != 200 {
		t.Fatalf("refused retry broke the previous revision: %d", status)
	}
}

func TestFailedUpdateSetupLeavesServerRunning(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	port := freePort(t)
	app := createServerAppOn(t, f, port)
	workers.mu.Lock()
	listener := workers.listeners["worker-app "+app.ID]
	workers.mu.Unlock()
	workers.wait = func(context.Context) (int, error) { return 1, nil }
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "serve", Port: port, Setup: "false", UploadID: upload, Digest: digest}); err == nil {
		t.Fatal("failed setup reported success")
	}
	if workers.wasStopped(app.ID) {
		t.Fatal("failed setup stopped the working server")
	}
	workers.mu.Lock()
	current := workers.listeners["worker-app "+app.ID]
	workers.mu.Unlock()
	if current != listener {
		t.Fatal("failed setup replaced the working server's listener")
	}
	if a := f.origin.state.Apps[app.ID]; a.Phase != "ready" || a.Session != "worker-app "+app.ID {
		t.Fatalf("failed setup lost the working server's record: %#v", a)
	}
}

func TestDeleteWithMissingWorkloadRootCompletesCleanup(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatal(err)
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "delete", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	for _, restarted := range []bool{false, true} {
		if restarted {
			restartAppOrigin(t, f)
		}
		if _, ok := f.origin.state.Apps[app.ID]; ok {
			t.Fatalf("app record remains after acknowledged cleanup (restarted %v)", restarted)
		}
		for token, receipt := range f.origin.state.Receipts {
			if receipt.Record.ID == app.ID {
				t.Fatalf("receipt %s remains after acknowledged cleanup (restarted %v)", token, restarted)
			}
		}
	}
	requireEdgeCleanup(t, f, app.ID)
}

// outputWorkers fails setup with an exit code and has output to show for it.
type outputWorkers struct {
	*fakeWorkers
	mu     sync.Mutex
	setup  string
	output string
}

func (w *outputWorkers) Start(ctx context.Context, label, command, root string, env []string) (string, error) {
	if id, ok := strings.CutPrefix(label, "app-setup "); ok {
		w.mu.Lock()
		w.setup = id
		w.mu.Unlock()
	}
	return w.fakeWorkers.Start(ctx, label, command, root, env)
}
func (w *outputWorkers) Wait(context.Context, string) (int, error) { return 1, nil }
func (w *outputWorkers) Output(context.Context, string) string     { return w.output }

func TestFailedCreateKeepsSetupDiagnostic(t *testing.T) {
	f := newAppFixture(t)
	workers := &outputWorkers{fakeWorkers: f.workers, output: strings.Repeat("noise\n", 4096) + "npm ERR! missing script: build\n"}
	f.origin.config.Workers = workers
	upload, digest := uploadSource(t, f, sourceFixture(t))
	_, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", Setup: "npm run build", UploadID: upload, Digest: digest})
	id := workers.setup
	if err == nil || id == "" || !strings.Contains(err.Error(), id) {
		t.Fatalf("failed create did not name its app: %v", err)
	}
	restartAppOrigin(t, f)
	inspect := func() string {
		t.Helper()
		result, err := f.origin.Handle(context.Background(), Request{Action: "inspect", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(result)
		return string(raw)
	}
	shown := inspect()
	if !strings.Contains(shown, "npm ERR! missing script: build") || !strings.Contains(shown, "setup exited 1") {
		t.Fatalf("owner cannot see why setup failed: %s", shown)
	}
	if len(shown) > 16<<10 {
		t.Fatalf("diagnostic is unbounded: %d bytes", len(shown))
	}
	f.now = f.now.Add(IdleTTL)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(inspect(), "missing script") {
		t.Fatal("diagnostic outlived its retention")
	}
}

// crashAfterActivatingStore keeps every save up to the update's activating
// write and loses all later ones, as if the daemon died right after it.
type crashAfterActivatingStore struct {
	*memoryAppStore
	crashed bool
}

func (s *crashAfterActivatingStore) SaveAppState(ctx context.Context, key string, data []byte) error {
	if key != "apps.origin" {
		return s.memoryAppStore.SaveAppState(ctx, key, data)
	}
	if s.crashed {
		return errors.New("daemon crashed")
	}
	var state originState
	_ = json.Unmarshal(data, &state)
	for _, a := range state.Apps {
		if a.Phase == "activating" {
			s.crashed = true
		}
	}
	return s.memoryAppStore.SaveAppState(ctx, key, data)
}

func TestRollbackIsDurableBeforeCandidateIsRemoved(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	port := freePort(t)
	app := createServerAppOn(t, f, port)
	failed := false
	workers.beforeStart = func(_ context.Context, _, command string) error {
		if command == "replacement" && !failed {
			failed = true
			return errors.New("replacement failed to start")
		}
		return nil
	}
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	f.origin.config.Store = &crashAfterActivatingStore{memoryAppStore: f.originStore}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "replacement", Port: port, UploadID: upload, Digest: digest}); err == nil {
		t.Fatal("replacement start failure was hidden")
	}
	restartAppOrigin(t, f)
	f.origin.config.Workers = workers
	_ = f.origin.Sync(context.Background())
	a := f.origin.state.Apps[app.ID]
	if a.Phase != "ready" {
		t.Fatalf("recovery left the app %s", a.Phase)
	}
	if _, err := os.Stat(filepath.Join(a.Root, "index.html")); err != nil {
		t.Fatalf("recovery serves a workspace the rollback already deleted: %v", err)
	}
}

func TestFailedReplacementStopIsFinishedByRecovery(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	port := freePort(t)
	app := createServerAppOn(t, f, port)
	var mu sync.Mutex
	var started []string
	failing := false
	workers.beforeStart = func(_ context.Context, label, command string) error {
		mu.Lock()
		defer mu.Unlock()
		if label == "app "+app.ID {
			started = append(started, command)
			failing = command == "replacement"
		}
		return nil
	}
	workers.beforeFind = func(_ context.Context, label string) error {
		mu.Lock()
		defer mu.Unlock()
		if failing && label == "app "+app.ID {
			return errors.New("catalog unavailable")
		}
		return nil
	}
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "replacement", Port: port, UploadID: upload, Digest: digest}); err == nil {
		t.Fatal("replacement validation failure was hidden")
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	restartAppOrigin(t, f)
	f.origin.config.Workers = workers
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	last := started[len(started)-1]
	mu.Unlock()
	if last != "serve" || !workers.alive(t, "app "+app.ID) {
		t.Fatalf("recovery left the rejected replacement in place of the previous server: starts %v", started)
	}
}

// persistingSetupWorkers runs a hook as setup starts, before Start returns.
type persistingSetupWorkers struct {
	*fakeWorkers
	starting func()
}

func (w *persistingSetupWorkers) Start(ctx context.Context, label, command, root string, env []string) (string, error) {
	if strings.HasPrefix(label, "app-setup ") {
		w.starting()
	}
	return w.fakeWorkers.Start(ctx, label, command, root, env)
}

// TestPublishedCandidateIsNeverMutated guards persistence against a data race:
// other apps' saves serialize the candidate under the state lock, so once a
// candidate is reachable from state it must be replaced, never written.
func TestPublishedCandidateIsNeverMutated(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	var published *updateCandidate
	var snapshot updateCandidate
	f.origin.config.Workers = &persistingSetupWorkers{fakeWorkers: f.workers, starting: func() {
		f.origin.mu.Lock()
		defer f.origin.mu.Unlock()
		published = f.origin.state.Apps[app.ID].Candidate
		snapshot = *published
	}}
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "static", Setup: "npm ci", UploadID: upload, Digest: digest}); err != nil {
		t.Fatal(err)
	}
	if published.Session != snapshot.Session {
		t.Fatalf("setup wrote session %q into a candidate other saves could be serializing", published.Session)
	}
}

var errUnmounted = errors.New("app: required data SSD /work is not mounted")

func TestCandidateRollbackWaitsForStorage(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	crashInSetup(t, f, Request{Action: "update", ID: app.ID, Kind: "static", Setup: "npm ci", UploadID: upload, Digest: digest})
	f.origin.storageMounted = func(string) error { return errUnmounted }
	_ = f.origin.Sync(context.Background())
	if f.origin.state.Apps[app.ID].Candidate == nil {
		t.Fatal("rollback forgot the interrupted update while its storage was unmounted")
	}
	f.origin.storageMounted = workloadStorageMounted
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.origin.state.Apps[app.ID].Candidate != nil {
		t.Fatal("rollback did not finish once storage returned")
	}
}

func TestCleanupWaitsForStorageWhenRootStillExists(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	f.origin.storageMounted = func(string) error { return errUnmounted }
	if _, err := f.origin.Handle(context.Background(), Request{Action: "delete", ID: app.ID}); err == nil {
		t.Fatal("delete acknowledged cleanup while storage was unmounted")
	}
	if _, ok := f.origin.state.Apps[app.ID]; !ok {
		t.Fatal("delete dropped the app record while storage was unmounted")
	}
	record, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || record.Cleanup == "complete" {
		t.Fatalf("edge was told cleanup finished while storage was unmounted: %#v %v", record, err)
	}
}

func TestInterruptedUpdateRefusesChangedRecipeAfterRecovery(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	q := Request{Action: "update", ID: app.ID, Kind: "static", Setup: "npm ci", UploadID: upload, Digest: digest}
	crashInSetup(t, f, q)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	q.Setup = "npm install"
	_, err := f.origin.Handle(context.Background(), q)
	if err == nil || !strings.Contains(err.Error(), "conflicts") || !strings.Contains(err.Error(), app.ID) || !strings.Contains(err.Error(), upload) {
		t.Fatalf("changed recipe was accepted after recovery rolled the attempt back: %v", err)
	}
}
