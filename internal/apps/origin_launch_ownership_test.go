package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type ownershipWorkers struct {
	*serverWorkers
	lock             sync.Mutex
	hide             bool
	launchErr        error
	stopErr          error
	releaseErr       error
	ownerFailures    int
	ownerMissing     bool
	stopIgnoreCancel bool
	starts           int
	owners           int
	stops            []string
	releases         []string
	events           []string
	roots            map[string]string
	beforeStop       func(string)
	afterOwner       func(string)
	afterStart       func(context.Context, string, string)
	waitFor          func(context.Context) (int, error)
	beforeForget     func(string)
}

func ownershipFixture(t *testing.T) (*appFixture, *ownershipWorkers) {
	t.Helper()
	f := newAppFixture(t)
	w := &ownershipWorkers{serverWorkers: newServerWorkers(t, f), roots: map[string]string{}}
	f.origin.config.Workers = w
	t.Cleanup(func() {
		f.origin.Close()
		f.origin.mu.Lock()
		defer f.origin.mu.Unlock()
		if len(f.origin.ops) != 0 || len(f.origin.fences) != 0 || len(f.origin.drains) != 0 {
			t.Errorf("ownership gates remain: ops %d fences %d drains %d", len(f.origin.ops), len(f.origin.fences), len(f.origin.drains))
		}
	})
	return f, w
}

func (w *ownershipWorkers) Start(ctx context.Context, label, command, root string, env []string) (string, error) {
	id, err := w.serverWorkers.Start(ctx, label, command, root, env)
	if err != nil {
		return id, err
	}
	w.lock.Lock()
	w.starts++
	w.roots[id] = root
	w.events = append(w.events, "start "+id)
	hook, launchErr := w.afterStart, w.launchErr
	w.lock.Unlock()
	if hook != nil {
		hook(ctx, label, id)
	}
	return id, launchErr
}
func (w *ownershipWorkers) Owner(ctx context.Context, id string) (string, error) {
	w.lock.Lock()
	w.owners++
	if w.ownerMissing {
		w.lock.Unlock()
		return "", ErrNoOwner
	}
	if w.ownerFailures > 0 {
		w.ownerFailures--
		w.lock.Unlock()
		return "", errors.New("owner read failed")
	}
	w.lock.Unlock()
	marker, err := w.fakeWorkers.Owner(ctx, id)
	if err == nil && w.afterOwner != nil {
		w.afterOwner(id)
	}
	return marker, err
}
func (w *ownershipWorkers) Find(ctx context.Context, label string) (string, bool, error) {
	w.lock.Lock()
	hide := w.hide
	w.lock.Unlock()
	if hide {
		return "", false, nil
	}
	return w.serverWorkers.Find(ctx, label)
}
func (w *ownershipWorkers) Stop(ctx context.Context, id string) error {
	w.lock.Lock()
	w.stops = append(w.stops, id)
	hook, err := w.beforeStop, w.stopErr
	w.lock.Unlock()
	if hook != nil {
		hook(id)
	}
	if err != nil {
		return err
	}
	if w.stopIgnoreCancel {
		ctx = context.WithoutCancel(ctx)
	}
	err = w.serverWorkers.Stop(ctx, id)
	w.lock.Lock()
	w.events = append(w.events, "stop returned "+id)
	w.lock.Unlock()
	return err
}
func (w *ownershipWorkers) Release(ctx context.Context, id, marker string) error {
	w.lock.Lock()
	w.releases = append(w.releases, marker)
	w.events = append(w.events, "release "+id)
	err := w.releaseErr
	w.lock.Unlock()
	if err != nil {
		return err
	}
	return w.fakeWorkers.Release(ctx, id, marker)
}
func (w *ownershipWorkers) Wait(ctx context.Context, id string) (int, error) {
	if w.waitFor != nil {
		return w.waitFor(ctx)
	}
	return w.serverWorkers.Wait(ctx, id)
}
func (w *ownershipWorkers) Forget(ctx context.Context, label string) {
	if w.beforeForget != nil {
		w.beforeForget(label)
	}
	w.lock.Lock()
	w.events = append(w.events, "forget "+label)
	w.lock.Unlock()
	w.serverWorkers.Forget(ctx, label)
}

func savedOwnership(t *testing.T, f *appFixture) originState {
	t.Helper()
	var state originState
	if err := load(context.Background(), f.originStore, "apps.origin", &state); err != nil {
		t.Fatal(err)
	}
	return state
}
func reopenOwnership(t *testing.T, f *appFixture, w *ownershipWorkers) {
	t.Helper()
	f.origin.Close()
	config := f.origin.config
	config.Store = f.originStore
	config.Workers = w
	origin, err := NewOrigin(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	f.origin = origin
	t.Cleanup(origin.Close)
}
func startOwnership(t *testing.T, f *appFixture, id, label string) launchedWorker {
	t.Helper()
	ctx, release, err := f.origin.beginOp(context.Background(), "app "+id, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := f.origin.start(ctx, id, label, "setup", f.root, nil); err != nil {
		t.Fatal(err)
	}
	claims := f.origin.claims(id, label)
	if len(claims) != 1 {
		t.Fatalf("claims = %+v", claims)
	}
	return claims[0]
}
func assertNoClaims(t *testing.T, f *appFixture, id string) {
	t.Helper()
	if claims := savedOwnership(t, f).Launched[id]; len(claims) != 0 {
		t.Fatalf("saved claims remain: %+v", claims)
	}
}
func requirePath(t *testing.T, path string, exists bool) {
	t.Helper()
	_, err := os.Stat(path)
	if exists && err != nil {
		t.Fatalf("required path %s: %v", path, err)
	}
	if !exists && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed path %s: %v", path, err)
	}
}
func createOwnershipFailure(t *testing.T, kind string) {
	t.Helper()
	f, w := ownershipFixture(t)
	sentinel := errors.New("post-launch publication failure")
	w.launchErr, w.hide = sentinel, true
	w.beforeStop = func(id string) { requirePath(t, filepath.Join(w.roots[id], "index.html"), true) }
	w.beforeForget = func(label string) {
		assertNoClaims(t, f, strings.TrimPrefix(strings.TrimPrefix(label, "app-setup "), "app "))
	}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	request := Request{Action: "create", Kind: kind, UploadID: upload, Digest: digest, Setup: "setup"}
	if kind == "server" {
		request.Setup, request.Command, request.Port = "", "server", freePort(t)
	}
	_, err := f.origin.Handle(context.Background(), request)
	if !errors.Is(err, sentinel) {
		t.Fatalf("create = %v", err)
	}
	if len(w.stops) != 1 || len(w.releases) != 1 {
		t.Fatalf("stops %v releases %v", w.stops, w.releases)
	}
	for _, root := range w.roots {
		requirePath(t, root, false)
	}
	state := savedOwnership(t, f)
	if len(state.Apps) != 0 || len(state.Launched) != 0 {
		t.Fatalf("rollback saved state = %+v", state)
	}
}
func TestLaunchOwnershipStopsBeforeDelete(t *testing.T)   { createOwnershipFailure(t, "static") }
func TestLaunchOwnershipServerCreateFailure(t *testing.T) { createOwnershipFailure(t, "server") }

func TestLaunchOwnershipRecordedBeforeOriginStartReturns(t *testing.T) {
	for _, prefix := range []string{"app ", "app-setup "} {
		t.Run(strings.TrimSpace(prefix), func(t *testing.T) { ownershipRecorded(t, prefix) })
	}
}
func ownershipRecorded(t *testing.T, prefix string) {
	f, w := ownershipFixture(t)
	// A setup recipe avoids a real server listener while testing both labels.
	id := "test"
	ctx, release, err := f.origin.beginOp(context.Background(), "app "+id, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	command := "setup"
	if prefix == "app " {
		command = "server"
	}
	session, err := f.origin.start(ctx, id, prefix+id, command, f.root, []string{"PORT=0"})
	if err != nil {
		t.Fatal(err)
	}
	claims := savedOwnership(t, f).Launched[id]
	marker, err := w.Owner(ctx, session)
	if err != nil || len(claims) != 1 || claims[0].marker() != marker || claims[0].Ended {
		t.Fatalf("start not durably recorded: %+v marker %q err %v", claims, marker, err)
	}
	if prefix == "app " {
		return
	}
	if err := f.origin.waitSetup(ctx, id, session); err != nil {
		t.Fatal(err)
	}
	if err := f.origin.drainEnded(ctx, id); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t, f, id)
	w.Forget(ctx, prefix+id)
}

func TestLaunchOwnershipEmptyIDFailure(t *testing.T) {
	f, w := ownershipFixture(t)
	sentinel := errors.New("before spawn")
	w.beforeStart = func(context.Context, string, string) error { return sentinel }
	upload, digest := uploadSource(t, f, sourceFixture(t))
	_, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest, Setup: "setup"})
	if !errors.Is(err, sentinel) || w.owners != 0 || len(savedOwnership(t, f).Launched) != 0 {
		t.Fatalf("pre-start failure: %v owner reads %d", err, w.owners)
	}
}

func TestLaunchOwnershipFailedUpdateBeforeSwap(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createServerAppOn(t, f, freePort(t))
	old := f.origin.state.Apps[app.ID]
	sentinel := errors.New("setup publication")
	w.launchErr, w.hide = sentinel, true
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: old.Command, Port: old.Port, Setup: "setup", UploadID: upload, Digest: digest})
	if !errors.Is(err, sentinel) {
		t.Fatalf("update = %v", err)
	}
	restored := savedOwnership(t, f).Apps[app.ID]
	if restored.Root != old.Root || restored.Session != old.Session || restored.Candidate != nil || slices.Contains(w.stops, old.Session) {
		t.Fatalf("pre-swap server lost: old %+v new %+v stops %v", old, restored, w.stops)
	}
	if len(w.stops) != 1 {
		t.Fatalf("setup stops = %v", w.stops)
	}
}
func TestLaunchOwnershipFailedUpdateAfterSwap(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createServerAppOn(t, f, freePort(t))
	old := f.origin.state.Apps[app.ID]
	sentinel := errors.New("replacement publication")
	w.afterStart = func(_ context.Context, label, session string) {
		if label != "app "+app.ID {
			return
		}
		w.lock.Lock()
		w.launchErr = nil
		w.lock.Unlock()
	}
	w.launchErr = sentinel
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: old.Command, Port: old.Port, UploadID: upload, Digest: digest})
	if !errors.Is(err, sentinel) {
		t.Fatalf("update = %v", err)
	}
	restored := savedOwnership(t, f).Apps[app.ID]
	if restored.Root != old.Root || restored.Candidate != nil || w.starts != 3 || len(w.stops) != 2 {
		t.Fatalf("replacement rollback: restored %+v starts %d stops %v", restored, w.starts, w.stops)
	}
}

func TestLaunchOwnershipStopOutcomes(t *testing.T) {
	t.Run("concurrent-end-before-action", ownershipConcurrentEnd)
	for _, name := range []string{"confirmed-exit", "missing-metadata-and-socket", "empty-find", "find-error"} {
		t.Run(name, func(t *testing.T) { ownershipStopOutcome(t, name) })
	}
}
func ownershipStopOutcome(t *testing.T, name string) {
	f, w := ownershipFixture(t)
	claim := startOwnership(t, f, "test", "app-setup test")
	if name != "confirmed-exit" {
		w.stopErr = errors.New("exit unknown")
	}
	w.hide = name == "empty-find" || name == "missing-metadata-and-socket"
	if name == "find-error" {
		w.findErr[claim.Label] = errors.New("catalog unavailable")
	}
	err := f.origin.stop(context.Background(), "test")
	if name == "confirmed-exit" {
		if err != nil {
			t.Fatal(err)
		}
		assertNoClaims(t, f, "test")
		return
	}
	claims := f.origin.claims("test", "")
	if err == nil || len(claims) != 1 || claims[0].Ended || len(w.releases) != 0 {
		t.Fatalf("unknown exit discharged: %v %+v releases %v", err, claims, w.releases)
	}
}

func TestLaunchOwnershipIdentity(t *testing.T) {
	t.Run("bound-ended-replay", ownershipBoundReplay)
	for _, name := range []string{"other-app-label", "other-token", "ordinary-session", "missing-marker", "unreadable-marker", "initial-read-error-binds", "same-label-replay"} {
		t.Run(name, func(t *testing.T) { ownershipIdentity(t, name) })
	}
}
func ownershipIdentity(t *testing.T, name string) {
	f, w := ownershipFixture(t)
	if name == "initial-read-error-binds" {
		ownershipInitialBind(t, f, w)
		return
	}
	claim := startOwnership(t, f, "test", "app-setup test")
	marker := claim.marker()
	switch name {
	case "other-app-label":
		marker = "app-setup other\n00000000000000000000000000000001"
	case "other-token", "same-label-replay":
		marker = claim.Label + "\n00000000000000000000000000000002"
	case "ordinary-session":
		marker = "shell\n00000000000000000000000000000001"
	case "missing-marker":
		marker = ""
		w.ownerMissing = true
	case "unreadable-marker":
		w.ownerFailures = 2
	}
	w.fakeWorkers.mu.Lock()
	w.ownersMapSet(claim.ID, marker)
	w.fakeWorkers.mu.Unlock()
	w.hide = true
	err := f.origin.stop(context.Background(), "test")
	unknown := name == "missing-marker" || name == "unreadable-marker" || name == "ordinary-session"
	if unknown && err == nil {
		t.Fatal("unknown marker accepted")
	}
	if !unknown && err != nil {
		t.Fatal(err)
	}
	if len(w.stops) != 0 || len(w.releases) != 0 {
		t.Fatalf("different/unknown owner touched: stops %v releases %v", w.stops, w.releases)
	}
	if !unknown {
		assertNoClaims(t, f, "test")
	}
}
func (w *ownershipWorkers) ownersMapSet(id, marker string) {
	if marker == "" {
		delete(w.fakeWorkers.owners, id)
		return
	}
	w.fakeWorkers.owners[id] = marker
}

func TestLaunchOwnershipStopFailureRetainsAndRetries(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createStaticApp(t, f)
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	w.launchErr, w.stopErr, w.hide = errors.New("publication"), errors.New("stop uncertain"), true
	q := Request{Action: "update", ID: app.ID, Kind: "static", Setup: "setup", UploadID: upload, Digest: digest}
	if _, err := f.origin.Handle(context.Background(), q); err == nil {
		t.Fatal("expected failed update")
	}
	saved := savedOwnership(t, f).Apps[app.ID]
	if saved.Candidate == nil {
		t.Fatal("candidate intent lost")
	}
	requirePath(t, saved.Candidate.Root, true)
	starts := w.starts
	if _, err := f.origin.Handle(context.Background(), q); err == nil || w.starts != starts {
		t.Fatal("retry launched past uncertain stop")
	}
	w.stopErr, w.launchErr = nil, nil
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t, f, app.ID)
	requirePath(t, saved.Candidate.Root, false)
}

type ownershipFaultStore struct {
	*memoryAppStore
	fail   func(originState) bool
	before bool
}

func (s *ownershipFaultStore) SaveAppState(ctx context.Context, key string, data []byte) error {
	var state originState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode fault state: %w", err)
	}
	fault := key == "apps.origin" && s.fail != nil && s.fail(state)
	if fault {
		s.fail = nil
	}
	if fault && s.before {
		return errors.New("failed ownership write")
	}
	if err := s.memoryAppStore.SaveAppState(ctx, key, data); err != nil {
		return err
	}
	if fault {
		return errors.New("lost ownership acknowledgement")
	}
	return nil
}
func TestLaunchOwnershipRecordSaveFailure(t *testing.T) {
	f, w := ownershipFixture(t)
	f.origin.config.Store = &ownershipFaultStore{memoryAppStore: f.originStore, before: true, fail: func(s originState) bool { return len(s.Launched) != 0 }}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	if _, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest, Setup: "setup"}); err == nil {
		t.Fatal("save failure accepted")
	}
	if len(w.stops) != 1 {
		t.Fatalf("in-memory launch lost: %v", w.stops)
	}
	if len(savedOwnership(t, f).Apps) != 0 {
		t.Fatal("failed create not completed")
	}
}
func TestLaunchOwnershipEndedSaveFaults(t *testing.T) {
	for _, phase := range []string{"ended", "removal", "release"} {
		t.Run(phase, func(t *testing.T) { ownershipFaultPhase(t, phase) })
	}
}
func ownershipFaultPhase(t *testing.T, phase string) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprintf("before-%t", before), func(t *testing.T) { ownershipCompletionFault(t, phase, before) })
	}
}
func ownershipCompletionFault(t *testing.T, phase string, before bool) {
	f, w := ownershipFixture(t)
	app := createStaticApp(t, f)
	claim := startOwnership(t, f, app.ID, "app-setup "+app.ID)
	predicate := func(s originState) bool {
		claims := s.Launched[app.ID]
		if phase == "removal" {
			return len(claims) == 0
		}
		return len(claims) > 0 && claims[0].Ended
	}
	f.origin.config.Store = &ownershipFaultStore{memoryAppStore: f.originStore, before: before, fail: predicate}
	if phase == "release" {
		f.origin.config.Store = f.originStore
		w.releaseErr = errors.New("release uncertain")
	}
	if err := f.origin.stop(context.Background(), app.ID); err == nil {
		t.Fatal("completion fault lost")
	}
	durable := savedOwnership(t, f).Launched[app.ID]
	if phase == "ended" && before && (len(durable) != 1 || durable[0].Ended) {
		t.Fatalf("failed write modified durable running claim: %+v", durable)
	}
	if phase == "ended" && !before && (len(durable) != 1 || !durable[0].Ended || durable[0].Token != claim.Token) {
		t.Fatalf("lost ack did not commit exact end: %+v", durable)
	}
	// Same-process retry must use the retained generation, even after a lost ack.
	if phase == "removal" {
		if err := f.origin.drainEnded(context.Background(), app.ID); err != nil {
			t.Fatal(err)
		}
	}
	stops := len(w.stops)
	reopenOwnership(t, f, w)
	w.releaseErr = nil
	if err := f.origin.stop(context.Background(), app.ID); err != nil {
		t.Fatal(err)
	}
	if !(phase == "ended" && before) && len(w.stops) != stops {
		t.Fatal("ended entry was stopped again")
	}
	assertNoClaims(t, f, app.ID)
}

func TestLaunchOwnershipRestartRecovery(t *testing.T) {
	t.Run("activating-found", func(t *testing.T) { ownershipActivating(t, false) })
	t.Run("activating-unfound", func(t *testing.T) { ownershipActivating(t, true) })
	t.Run("healthy-repeated", ownershipHealthyRestarts)
	t.Run("healthy-adoption-drain", ownershipHealthyAdoption)
	for _, phase := range []string{"preparing-create", "ready-update-candidate", "activating-server"} {
		t.Run(phase, func(t *testing.T) { ownershipRecovery(t, phase) })
	}
}
func ownershipRecovery(t *testing.T, phase string) {
	f, w := ownershipFixture(t)
	app := createStaticApp(t, f)
	root := f.origin.state.Apps[app.ID].Root
	claim := startOwnership(t, f, app.ID, "app-setup "+app.ID)
	switch phase {
	case "preparing-create":
		if err := f.origin.modify(context.Background(), app.ID, func(a *localApp) { a.Phase = "preparing" }); err != nil {
			t.Fatal(err)
		}
	case "ready-update-candidate":
		candidate := filepath.Join(f.root, "candidate")
		if err := os.Mkdir(candidate, 0700); err != nil {
			t.Fatal(err)
		}
		if err := f.origin.modify(context.Background(), app.ID, func(a *localApp) { a.Candidate = &updateCandidate{Root: candidate, UploadID: "interrupted"} }); err != nil {
			t.Fatal(err)
		}
	case "activating-server":
		f.origin.markEnded(app.ID, claim)
		if err := f.origin.persist(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	reopenOwnership(t, f, w)
	w.hide = true
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase == "preparing-create" {
		requirePath(t, root, true)
		if err := f.origin.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		requirePath(t, root, false)
	}
	assertNoClaims(t, f, app.ID)
}
func TestLaunchOwnershipSetupInterruptedRecovery(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createStaticApp(t, f)
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	w.waitFor = func(context.Context) (int, error) { panic(errSetupCrash) }
	crashOwnershipUpdate(t, f, Request{Action: "update", ID: app.ID, Kind: "static", Setup: "setup", UploadID: upload, Digest: digest})
	candidate := savedOwnership(t, f).Apps[app.ID].Candidate
	if candidate == nil {
		t.Fatal("candidate missing")
	}
	reopenOwnership(t, f, w)
	w.waitFor = nil
	w.hide = true
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	requirePath(t, candidate.Root, false)
	assertNoClaims(t, f, app.ID)
	if len(w.stops) != 1 {
		t.Fatalf("setup not reached by recorded identity %v", w.stops)
	}
}
func TestLaunchOwnershipCancelledCaller(t *testing.T) {
	f, w := ownershipFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.afterStart = func(context.Context, string, string) { cancel() }
	upload, digest := uploadSource(t, f, sourceFixture(t))
	if _, err := f.origin.Handle(ctx, Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest, Setup: "setup"}); err == nil {
		t.Fatal("cancelled operation succeeded")
	}
	if len(w.stops) != 1 {
		t.Fatalf("cancelled launch lost: %v", w.stops)
	}
	if len(savedOwnership(t, f).Apps) != 0 {
		t.Fatal("cancelled create not cleaned")
	}
}

func TestLaunchOwnershipStaleRemovalKeepsNewerEntry(t *testing.T) {
	f, _ := ownershipFixture(t)
	old := launchedWorker{ID: "X", Label: "app test", Token: "old", Gen: 1, Ended: true}
	newer := old
	newer.Gen = 2
	newer.Token = "new"
	f.origin.state.Launched = map[string][]launchedWorker{"test": {newer}}
	f.origin.mu.Lock()
	f.origin.removeClaimLocked("test", old)
	f.origin.mu.Unlock()
	if got := f.origin.claims("test", ""); len(got) != 1 || got[0] != newer {
		t.Fatalf("stale remover touched generation: %+v", got)
	}
}
func TestLaunchOwnershipRecordedAndDiscoveredStopOnce(t *testing.T) {
	f, w := ownershipFixture(t)
	claim := startOwnership(t, f, "test", "app-setup test")
	if err := f.origin.stop(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.stops, []string{claim.ID}) {
		t.Fatalf("duplicate stop %v", w.stops)
	}
}
func TestLaunchOwnershipWholeRecordRestoreKeepsEntries(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createStaticApp(t, f)
	old := f.origin.state.Apps[app.ID]
	w.launchErr, w.stopErr = errors.New("publication failed"), errors.New("stop unknown")
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "static", Setup: "setup", UploadID: upload, Digest: digest})
	state := savedOwnership(t, f)
	if err == nil || state.Apps[app.ID].Root != old.Root || state.Apps[app.ID].Candidate == nil {
		t.Fatalf("restore state %+v error %v", state.Apps[app.ID], err)
	}
	claims := state.Launched[app.ID]
	if len(claims) != 1 || claims[0].Ended || claims[0].Token == "" {
		t.Fatalf("whole-record restore lost claim: %+v", claims)
	}
}
func TestLaunchOwnershipOneRunningEntryPerLabel(t *testing.T) {
	f, w := ownershipFixture(t)
	claim := startOwnership(t, f, "test", "app-setup test")
	w.stopErr = errors.New("still running")
	for range 4 {
		if _, err := f.origin.start(context.Background(), "test", claim.Label, "setup", f.root, nil); err == nil {
			t.Fatal("replacement passed running gate")
		}
	}
	if w.starts != 1 || len(f.origin.claims("test", claim.Label)) != 1 {
		t.Fatal("running attempts accumulated")
	}
}
func TestLaunchOwnershipFoundUnrecordedReleasesMarker(t *testing.T) {
	t.Run("mismatched-found", ownershipFoundMismatch)
	t.Run("fenced-found", ownershipFoundFence)
	f, w := ownershipFixture(t)
	session, err := w.Start(context.Background(), "app-setup test", "setup", f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := w.Owner(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	w.beforeStop = func(id string) {
		claims := f.origin.claims("test", "")
		if len(claims) != 1 || claims[0].marker() != marker {
			t.Errorf("found action lacks reservation: %+v", claims)
		}
	}
	if err := f.origin.stop(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t, f, "test")
	if !slices.Equal(w.releases, []string{marker}) {
		t.Fatalf("found marker not released exactly: %v", w.releases)
	}
}

func waitOwnershipSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ownership barrier timed out")
	}
}
func ownershipResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("ownership operation did not join")
		return nil
	}
}
func TestLaunchOwnershipFencedSafetyStop(t *testing.T) {
	t.Run("revoked-during-gate", ownershipRevokedGate)
	f, w := ownershipFixture(t)
	app := createStaticApp(t, f)
	f.origin.yieldBudget = 20 * time.Millisecond
	enteredWait, releaseWait := make(chan struct{}), make(chan struct{})
	enteredStop, releaseStop := make(chan struct{}), make(chan struct{})
	enteredJoin, releaseJoin := make(chan struct{}), make(chan struct{})
	var waitOnce, stopCalls atomic.Int32
	var closeWait, closeStop, closeJoin sync.Once
	t.Cleanup(func() {
		closeWait.Do(func() { close(releaseWait) })
		closeStop.Do(func() { close(releaseStop) })
		closeJoin.Do(func() { close(releaseJoin) })
	})
	w.waitFor = func(ctx context.Context) (int, error) {
		if waitOnce.Add(1) != 1 {
			return 0, nil
		}
		close(enteredWait)
		<-releaseWait
		return -1, ctx.Err()
	}
	w.beforeStop = func(string) {
		switch stopCalls.Add(1) {
		case 1:
			close(enteredStop)
			<-releaseStop
		case 2:
			close(enteredJoin)
			<-releaseJoin
		}
	}
	upload, digest := uploadSource(t, f, updatedSourceFixture(t))
	updateA := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "static", Setup: "setup", UploadID: upload, Digest: digest})
		updateA <- err
	}()
	waitOwnershipSignal(t, enteredWait)
	var offline atomic.Bool
	offline.Store(true)
	exchange := f.origin.config.Exchange
	f.origin.config.Exchange = func(ctx context.Context, s Signed) (Signed, error) {
		if offline.Load() {
			return Signed{}, errors.New("registry offline")
		}
		return exchange(ctx, s)
	}
	if err := f.origin.modify(context.Background(), app.ID, func(a *localApp) { a.Record.LeaseUntil = f.now.Add(-time.Second) }); err != nil {
		t.Fatal(err)
	}
	safety := make(chan error, 1)
	go func() { safety <- f.origin.Sync(context.Background()) }()
	waitOwnershipSignal(t, enteredStop)
	join := make(chan error, 1)
	go func() { join <- f.origin.guardLapsed(context.Background(), app.ID) }()
	waitOwnershipSignal(t, enteredJoin)
	if err := f.origin.drainEnded(context.Background(), app.ID); err == nil {
		t.Fatal("release crossed safety fence")
	}
	waiter, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, release, err := f.origin.beginOp(waiter, "app "+app.ID, "cancelled waiter")
	cancel()
	if err == nil {
		release()
		t.Fatal("cancelled waiter acquired fenced app")
	}
	closeWait.Do(func() { close(releaseWait) })
	if err := ownershipResult(t, updateA); err == nil {
		t.Fatal("revoked update succeeded")
	}
	offline.Store(false)
	if _, err := f.origin.renewLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	uploadB, digestB := uploadSource(t, f, updatedSourceFixture(t))
	before := w.starts
	portB := freePort(t)
	updateB := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "server", Command: "serve", Port: portB, UploadID: uploadB, Digest: digestB})
		updateB <- err
	}()
	select {
	case err := <-updateB:
		t.Fatalf("fresh update overtook safety stop: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	closeStop.Do(func() { close(releaseStop) })
	if err := ownershipResult(t, safety); err == nil {
		t.Fatal("non-yielding safety result omitted")
	}
	select {
	case err := <-updateB:
		t.Fatalf("fresh update overtook joined safety stop: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	w.lock.Lock()
	starts := w.starts
	w.lock.Unlock()
	if starts != before {
		t.Fatal("launch crossed joined safety stop")
	}
	closeJoin.Do(func() { close(releaseJoin) })
	if err := ownershipResult(t, join); err == nil {
		t.Fatal("joined safety result omitted")
	}
	if err := ownershipResult(t, updateB); err != nil {
		t.Fatal(err)
	}
	w.lock.Lock()
	events := slices.Clone(w.events)
	w.lock.Unlock()
	lastStop, lastStart := -1, -1
	for i, event := range events {
		if strings.HasPrefix(event, "stop returned ") {
			lastStop = i
		}
		if strings.HasPrefix(event, "start ") {
			lastStart = i
		}
	}
	if lastStart <= lastStop {
		t.Fatalf("replacement preceded completed safety actions: %v", events)
	}
	claims := savedOwnership(t, f).Launched[app.ID]
	if len(claims) != 1 || claims[0].Ended || claims[0].Label != "app "+app.ID {
		t.Fatalf("fresh server claim lost: %+v", claims)
	}
	if _, alive, err := w.Find(context.Background(), "app "+app.ID); err != nil || !alive {
		t.Fatalf("fresh server not alive: %t %v", alive, err)
	}
}

func ownershipActivating(t *testing.T, hide bool) {
	f, w := ownershipFixture(t)
	exchange := f.origin.config.Exchange
	unavailable := true
	f.origin.config.Exchange = func(ctx context.Context, s Signed) (Signed, error) {
		var q Request
		if err := json.Unmarshal(s.Body, &q); err != nil {
			return Signed{}, fmt.Errorf("decode request: %w", err)
		}
		if q.Action == "activate" && unavailable {
			return Signed{}, errors.New("activation unavailable")
		}
		return exchange(ctx, s)
	}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	if _, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "server", Command: "serve", Port: freePort(t), UploadID: upload, Digest: digest}); err == nil {
		t.Fatal("activation fault lost")
	}
	var id string
	for key, a := range savedOwnership(t, f).Apps {
		if a.Phase != "activating" {
			t.Fatalf("phase %+v", a)
		}
		id = key
	}
	if id == "" {
		t.Fatal("no activating app")
	}
	unavailable = false
	reopenOwnership(t, f, w)
	w.hide = hide
	// A one-time discovery miss reaches the replacement gate. Its subsequent
	// readiness lookup sees the newly launched server.
	if hide {
		w.afterStart = func(context.Context, string, string) { w.lock.Lock(); w.hide = false; w.lock.Unlock() }
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	claims := savedOwnership(t, f).Launched[id]
	if len(claims) != 1 || claims[0].Ended || savedOwnership(t, f).Apps[id].Phase != "ready" {
		t.Fatalf("activation not settled: %+v", claims)
	}
	expected := 1
	if hide {
		expected = 2
	}
	if w.starts != expected || len(w.stops) != expected-1 {
		t.Fatalf("adopt/restart starts %d stops %v", w.starts, w.stops)
	}
}
func ownershipHealthyRestarts(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createServerApp(t, f)
	for range 3 {
		live := f.origin.claims(app.ID, "app "+app.ID)[0]
		if err := w.serverWorkers.Stop(context.Background(), live.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.origin.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		claims := savedOwnership(t, f).Launched[app.ID]
		if len(claims) != 1 || claims[0].Ended || claims[0].Token == live.Token {
			t.Fatalf("restart retained ended claim: %+v", claims)
		}
		w.fakeWorkers.mu.Lock()
		owners := len(w.fakeWorkers.owners)
		w.fakeWorkers.mu.Unlock()
		if owners != 1 {
			t.Fatalf("restart retained %d markers", owners)
		}
	}
}
func ownershipHealthyAdoption(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createServerApp(t, f)
	live := f.origin.claims(app.ID, "app "+app.ID)[0]
	setup := startOwnership(t, f, app.ID, "app-setup "+app.ID)
	if err := w.serverWorkers.Stop(context.Background(), setup.ID); err != nil {
		t.Fatal(err)
	}
	f.origin.markEnded(app.ID, setup)
	if err := f.origin.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopenOwnership(t, f, w)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if claims := savedOwnership(t, f).Launched[app.ID]; !slices.Equal(claims, []launchedWorker{live}) {
		t.Fatalf("healthy adoption retained old claim: %+v", claims)
	}
	if slices.Contains(w.releases, live.marker()) {
		t.Fatal("adoption unpinned serving worker")
	}
}

func ownershipInitialBind(t *testing.T, f *appFixture, w *ownershipWorkers) {
	w.ownerFailures = 1
	_, err := f.origin.start(context.Background(), "test", "app-setup test", "setup", f.root, nil)
	if err == nil {
		t.Fatal("initial read error lost")
	}
	if claim := savedOwnership(t, f).Launched["test"][0]; claim.Token != "" {
		t.Fatal("failed read granted authority")
	}
	if err := f.origin.stop(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t, f, "test")
	if len(w.stops) != 1 || len(w.releases) != 1 || !strings.HasSuffix(w.releases[0], "00000000000000000000000000000001") {
		t.Fatalf("bound completion %v %v", w.stops, w.releases)
	}
}
func ownershipPanicIs(value any, want error) bool {
	err, ok := value.(error)
	return ok && errors.Is(err, want)
}

func ownershipBoundReplay(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createStaticApp(t, f)
	w.ownerFailures = 1
	session, err := f.origin.start(context.Background(), app.ID, "app-setup "+app.ID, "setup", f.root, nil)
	if err == nil {
		t.Fatal("initial read failure lost")
	}
	f.origin.config.Store = &ownershipFaultStore{memoryAppStore: f.originStore, fail: func(s originState) bool { return len(s.Launched[app.ID]) == 1 && s.Launched[app.ID][0].Ended }}
	if err := f.origin.stop(context.Background(), app.ID); err == nil {
		t.Fatal("ended acknowledgement failure lost")
	}
	ended := savedOwnership(t, f).Launched[app.ID][0]
	if !ended.Ended || ended.Token == "" {
		t.Fatalf("bound marker not durably ended: %+v", ended)
	}
	reopenOwnership(t, f, w)
	replacement := ended.Label + "\nffffffffffffffffffffffffffffffff"
	w.fakeWorkers.mu.Lock()
	w.fakeWorkers.owners[session] = replacement
	w.fakeWorkers.mu.Unlock()
	stops := len(w.stops)
	if err := f.origin.stop(context.Background(), app.ID); err != nil {
		t.Fatal(err)
	}
	if len(w.stops) != stops {
		t.Fatal("ended replay stopped replacement")
	}
	if got, err := w.Owner(context.Background(), session); err != nil || got != replacement {
		t.Fatalf("replay unpinned replacement: %q %v", got, err)
	}
	assertNoClaims(t, f, app.ID)
}
func ownershipFoundMismatch(t *testing.T) {
	f, w := ownershipFixture(t)
	session, err := w.Start(context.Background(), "app-setup test", "setup", f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.fakeWorkers.mu.Lock()
	w.fakeWorkers.owners[session] = "app other\nffffffffffffffffffffffffffffffff"
	w.fakeWorkers.mu.Unlock()
	if err := f.origin.stop(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if len(w.stops) != 0 || len(w.releases) != 0 {
		t.Fatal("unrecorded mismatched occupant touched")
	}
	assertNoClaims(t, f, "test")
}
func ownershipFoundFence(t *testing.T) {
	f, w := ownershipFixture(t)
	if _, err := w.Start(context.Background(), "app-setup test", "setup", f.root, nil); err != nil {
		t.Fatal(err)
	}
	f.origin.mu.Lock()
	release, _ := f.origin.fenceLocked("test")
	f.origin.mu.Unlock()
	if err := f.origin.stopWorkers(context.Background(), "test"); err != nil {
		release()
		t.Fatal(err)
	}
	if err := f.origin.drainEnded(context.Background(), "test"); err == nil {
		release()
		t.Fatal("fenced drain accepted")
	}
	if len(w.releases) != 0 {
		release()
		t.Fatal("safety stopper released marker")
	}
	release()
	ctx, done, err := f.origin.beginOp(context.Background(), "app test", "next holder")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if err := f.origin.drainEnded(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t, f, "test")
}

func ownershipRevokedGate(t *testing.T) {
	f, w := ownershipFixture(t)
	app := createStaticApp(t, f)
	claim := startOwnership(t, f, app.ID, "app-setup "+app.ID)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ctx, release, err := f.origin.beginOp(context.Background(), "app "+app.ID, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	w.stopIgnoreCancel = true
	w.beforeStop = func(string) { close(entered); <-resume }
	done := make(chan error, 1)
	joined := make(chan struct{})
	t.Cleanup(func() { once.Do(func() { close(resume) }); waitOwnershipSignal(t, joined) })
	go func() {
		defer close(joined)
		_, err := f.origin.start(ctx, app.ID, claim.Label, "setup", f.root, nil)
		done <- err
	}()
	waitOwnershipSignal(t, entered)
	f.origin.mu.Lock()
	op := f.origin.ops["app "+app.ID]
	op.revoked = "expired"
	op.cancel()
	f.origin.mu.Unlock()
	once.Do(func() { close(resume) })
	if err := ownershipResult(t, done); err == nil || !strings.Contains(err.Error(), "not starting") {
		t.Fatalf("slow gate did not recheck revocation: %v", err)
	}
	if w.starts != 1 {
		t.Fatalf("revoked gate launched %d workers", w.starts)
	}
}

func crashOwnershipUpdate(t *testing.T, f *appFixture, q Request) {
	t.Helper()
	defer func() {
		if r := recover(); !ownershipPanicIs(r, errSetupCrash) {
			t.Fatalf("unexpected panic %v", r)
		}
	}()
	_, _ = f.origin.Handle(context.Background(), q)
	t.Fatal("setup did not interrupt")
}

func ownershipConcurrentEnd(t *testing.T) {
	f, w := ownershipFixture(t)
	claim := startOwnership(t, f, "test", "app-setup test")
	entered, resume, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	w.afterOwner = func(string) {
		if calls.Add(1) == 1 {
			close(entered)
			<-resume
		}
	}
	done := make(chan error, 1)
	t.Cleanup(func() { once.Do(func() { close(resume) }); waitOwnershipSignal(t, joined) })
	go func() { defer close(joined); done <- f.origin.stopClaim(context.Background(), "test", claim) }()
	waitOwnershipSignal(t, entered)
	if err := f.origin.stopClaim(context.Background(), "test", claim); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(resume) })
	if err := ownershipResult(t, done); err != nil {
		t.Fatal(err)
	}
	if len(w.stops) != 1 {
		t.Fatalf("a stale snapshot stopped an already-ended claim again: %v", w.stops)
	}
	if err := f.origin.drainEnded(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t, f, "test")
}
