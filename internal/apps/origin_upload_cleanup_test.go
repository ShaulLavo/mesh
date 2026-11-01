package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExpiredUploadReclaimedWithRegistryOffline(t *testing.T) {
	t.Run("capacity", testExpiredUploadOfflineCapacity)
	t.Run("fresh upload and managed source", testExpiredUploadOfflineRetention)
	t.Run("canceled exchange", testExpiredUploadCanceledExchange)
}

func testExpiredUploadOfflineCapacity(t *testing.T) {
	t.Helper()
	f := newAppFixture(t)
	ids := make([]string, 32)
	for i := range ids {
		ids[i] = beginExpiryUpload(t, f)
	}
	f.now = f.now.Add(time.Hour)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin"}); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("33rd upload did not hit capacity: %v", err)
	}
	offline := errors.New("registry offline")
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, offline }
	if err := f.origin.Sync(context.Background()); !errors.Is(err, offline) {
		t.Fatalf("Sync hid the registry error: %v", err)
	}
	for _, id := range ids {
		requireExpiryUploadGone(t, f, id)
	}
	beginExpiryUpload(t, f)
}

func testExpiredUploadOfflineRetention(t *testing.T) {
	t.Helper()
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	source := filepath.Join(f.origin.state.Apps[app.ID].Root, "index.html")
	f.origin.state.Failures = map[string]SetupFailure{
		"expired": {ExpiresAt: f.now.Add(time.Hour)},
		"fresh":   {ExpiresAt: f.now.Add(2 * time.Hour)},
	}
	old := beginExpiryUpload(t, f)
	f.now = f.now.Add(30 * time.Minute)
	fresh := beginExpiryUpload(t, f)
	f.now = f.now.Add(30 * time.Minute)
	offline := errors.New("registry offline")
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, offline }
	if err := f.origin.Sync(context.Background()); !errors.Is(err, offline) {
		t.Fatalf("Sync hid the registry error: %v", err)
	}
	requireExpiryUploadGone(t, f, old)
	requireExpiryUploadPresent(t, f, fresh)
	if _, exists := f.origin.state.Failures["expired"]; exists || len(f.origin.state.Failures) != 1 {
		t.Fatalf("offline failure retention is incorrect: %#v", f.origin.state.Failures)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("outage cleanup removed managed source: %v", err)
	}
}

func TestExpiredUploadUnlinkFailureIsRetryable(t *testing.T) {
	f := newAppFixture(t)
	id := beginExpiryUpload(t, f)
	path := f.origin.uploadPath(id)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "obstacle"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	other := beginExpiryUpload(t, f)
	f.now = f.now.Add(time.Hour)
	err := f.origin.Sync(context.Background())
	var removal *os.PathError
	if !errors.As(err, &removal) || removal.Op != "remove" || removal.Path != path {
		t.Fatalf("Sync did not report the archive removal failure: %v", err)
	}
	requireExpiryUploadPresent(t, f, id)
	requireExpiryUploadSaved(t, f, id, true)
	requireExpiryUploadGone(t, f, other)
	requireExpiryUploadSaved(t, f, other, false)
	offline := errors.New("registry offline")
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, offline }
	err = f.origin.Sync(context.Background())
	if !errors.Is(err, offline) || !errors.As(err, &removal) {
		t.Fatalf("Sync did not join registry and removal errors: %v", err)
	}
	restartExpiryOrigin(t, f)
	requireExpiryUploadPresent(t, f, id)
	if err := os.Remove(filepath.Join(path, "obstacle")); err != nil {
		t.Fatal(err)
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireExpiryUploadGone(t, f, id)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatalf("repeated cleanup failed: %v", err)
	}
}

func beginExpiryUpload(t *testing.T, f *appFixture) string {
	t.Helper()
	result, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin"})
	if err != nil {
		t.Fatal(err)
	}
	return result.UploadID
}

func requireExpiryUploadGone(t *testing.T, f *appFixture, id string) {
	t.Helper()
	if _, ok := f.origin.state.Uploads[id]; ok {
		t.Fatalf("expired upload %s still occupies an admission slot", id)
	}
	if _, err := os.Stat(f.origin.uploadPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired archive %s remains: %v", id, err)
	}
}

func requireExpiryUploadPresent(t *testing.T, f *appFixture, id string) {
	t.Helper()
	if _, ok := f.origin.state.Uploads[id]; !ok {
		t.Fatalf("upload %s lost its retry record", id)
	}
	if _, err := os.Stat(f.origin.uploadPath(id)); err != nil {
		t.Fatalf("upload %s lost its archive: %v", id, err)
	}
}

func testExpiredUploadCanceledExchange(t *testing.T) {
	t.Helper()
	f := newAppFixture(t)
	id := beginExpiryUpload(t, f)
	store := &expiryStateStore{memoryAppStore: f.originStore, uploadID: id}
	store.check = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("expiry save context: %w", err)
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > maintenanceBudget {
			return errors.New("expiry save has no live maintenance budget")
		}
		return nil
	}
	f.origin.config.Store = store
	t.Cleanup(func() { f.origin.config.Store = f.originStore })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) {
		cancel()
		return Signed{}, ctx.Err()
	}
	f.now = f.now.Add(time.Hour)
	if err := f.origin.Sync(ctx); !errors.Is(err, context.Canceled) || store.saves != 1 {
		t.Fatalf("Sync did not save expiry after caller cancellation: saves %d, error %v", store.saves, err)
	}
	if store.checkErr != nil {
		t.Fatalf("expiry save received a spent or unbounded context: %v", store.checkErr)
	}
	requireExpiryUploadGone(t, f, id)
	requireExpiryUploadSaved(t, f, id, false)
}

func TestExpiredUploadKeepsActiveOperation(t *testing.T) {
	f := newAppFixture(t)
	id := beginExpiryUpload(t, f)
	ctx, release, err := f.origin.beginOp(context.Background(), "upload "+id, "create")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	f.now = f.now.Add(time.Hour)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("expiry canceled the active upload: %v", err)
	}
	requireExpiryUploadPresent(t, f, id)
	requireExpiryUploadSaved(t, f, id, true)
}

func TestExpiredUploadWaitsForStorage(t *testing.T) {
	f := newAppFixture(t)
	id := beginExpiryUpload(t, f)
	origin := f.origin
	mounted := origin.storageMounted
	t.Cleanup(func() { origin.storageMounted = mounted })
	origin.storageMounted = func(string) error { return errUnmounted }
	f.now = f.now.Add(time.Hour)
	if err := origin.Sync(context.Background()); !errors.Is(err, errUnmounted) {
		t.Fatalf("Sync hid the storage failure: %v", err)
	}
	requireExpiryUploadPresent(t, f, id)
	requireExpiryUploadSaved(t, f, id, true)
	origin.storageMounted = mounted
	restartExpiryOrigin(t, f)
	requireExpiryUploadPresent(t, f, id)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireExpiryUploadGone(t, f, id)
	requireExpiryUploadSaved(t, f, id, false)
}

func TestExpiredUploadSaveFailureSurvivesRestart(t *testing.T) {
	t.Run("failed write", func(t *testing.T) { testExpiredUploadSaveFailure(t, false) })
	t.Run("lost acknowledgement", func(t *testing.T) { testExpiredUploadSaveFailure(t, true) })
}

func testExpiredUploadSaveFailure(t *testing.T, after bool) {
	t.Helper()
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	receipt := f.origin.state.Receipts[app.Revision]
	id := beginExpiryUpload(t, f)
	failed := errors.New("expiry save unavailable")
	store := &expiryStateStore{memoryAppStore: f.originStore, uploadID: id, fail: failed, after: after}
	f.origin.config.Store = store
	f.now = f.now.Add(time.Hour)
	if err := f.origin.expireUploads(context.Background()); !errors.Is(err, failed) || store.saves != 1 {
		t.Fatalf("expiry did not reach its save failure: saves %d, error %v", store.saves, err)
	}
	requireExpiryUploadGone(t, f, id)
	requireExpiryUploadSaved(t, f, id, !after)
	restartExpiryOrigin(t, f)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatalf("missing-archive retry failed: %v", err)
	}
	requireExpiryUploadGone(t, f, id)
	requireExpiryUploadSaved(t, f, id, false)
	if got := f.origin.state.Receipts[app.Revision]; got != receipt {
		t.Fatalf("expiry changed the consumed-upload receipt: %#v", got)
	}
	requireExpiryUploadGone(t, f, app.Revision)
	if _, err := os.Stat(f.origin.state.Apps[app.ID].Root); err != nil {
		t.Fatalf("expiry retry deleted managed source: %v", err)
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatalf("repeated expiry retry failed: %v", err)
	}
}

func TestExpiredUploadPreservesActivationConsumers(t *testing.T) {
	t.Run("commit", testExpiredUploadCommitConsumer)
	t.Run("candidate", testExpiredUploadCandidateConsumer)
}

func testExpiredUploadCommitConsumer(t *testing.T) {
	t.Helper()
	f := newAppFixture(t)
	id, digest := uploadSource(t, f, sourceFixture(t))
	lost := errors.New("lost activation acknowledgement")
	f.origin.config.Exchange = func(ctx context.Context, signed Signed) (Signed, error) {
		response, err := f.edge.Exchange(ctx, signed)
		var request Request
		if decodeErr := json.Unmarshal(signed.Body, &request); decodeErr != nil {
			return Signed{}, fmt.Errorf("decode registry fixture request: %w", decodeErr)
		}
		if err == nil && request.Action == "activate" {
			return Signed{}, lost
		}
		return response, err
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "create", UploadID: id, Digest: digest}); !errors.Is(err, lost) {
		t.Fatalf("activation did not lose its acknowledgement: %v", err)
	}
	var pending localApp
	for _, app := range f.origin.state.Apps {
		pending = app
	}
	if pending.Commit == nil || pending.Commit.UploadID != id || pending.Phase != "activating" {
		t.Fatalf("fixture has no pending commit: %#v", pending)
	}
	expireUploadWithConsumerOffline(t, f, id, pending)
	restartExpiryOrigin(t, f)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatalf("activation could not finish without its raw archive: %v", err)
	}
	if f.origin.state.Apps[pending.Record.ID].Commit != nil || f.origin.state.Receipts[id].Record.ID != pending.Record.ID {
		t.Fatal("activation lost its receipt or retained its commit")
	}
	requireExpiryUploadGone(t, f, id)
}

func testExpiredUploadCandidateConsumer(t *testing.T) {
	t.Helper()
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	id, digest := uploadSource(t, f, updatedSourceFixture(t))
	origin := f.origin
	t.Cleanup(func() { origin.config.Workers = f.workers })
	origin.config.Workers = &crashingSetupWorkers{fakeWorkers: f.workers}
	crashExpiryCandidate(t, origin, Request{Action: "update", ID: app.ID, Setup: "fixture setup", UploadID: id, Digest: digest})
	origin.config.Workers = f.workers
	restartExpiryOrigin(t, f)
	pending := f.origin.state.Apps[app.ID]
	if pending.Candidate == nil || pending.Candidate.UploadID != id {
		t.Fatalf("fixture has no pending candidate: %#v", pending)
	}
	expireUploadWithConsumerOffline(t, f, id, pending)
	restartExpiryOrigin(t, f)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatalf("candidate rollback could not finish without its raw archive: %v", err)
	}
	if f.origin.state.Apps[app.ID].Candidate != nil {
		t.Fatal("recovery retained the interrupted candidate")
	}
	if _, err := os.Stat(pending.Candidate.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate rollback retained its workspace: %v", err)
	}
	if _, err := os.Stat(pending.Root); err != nil {
		t.Fatalf("candidate rollback removed managed source: %v", err)
	}
}

func expireUploadWithConsumerOffline(t *testing.T, f *appFixture, id string, pending localApp) {
	t.Helper()
	offline := errors.New("registry offline")
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, offline }
	f.now = f.now.Add(time.Hour)
	if err := f.origin.Sync(context.Background()); !errors.Is(err, offline) {
		t.Fatalf("Sync hid the registry error: %v", err)
	}
	requireExpiryUploadGone(t, f, id)
	current := f.origin.state.Apps[pending.Record.ID]
	if !reflect.DeepEqual(current, pending) {
		t.Fatalf("raw upload expiry changed its managed consumer: got %#v, want %#v", current, pending)
	}
	if _, err := os.Stat(pending.Root); err != nil {
		t.Fatalf("raw upload expiry removed managed source: %v", err)
	}
	if pending.Candidate != nil {
		if _, err := os.Stat(pending.Candidate.Root); err != nil {
			t.Fatalf("raw upload expiry removed its pending candidate: %v", err)
		}
	}
}

func restartExpiryOrigin(t *testing.T, f *appFixture) {
	t.Helper()
	f.origin.Close()
	restartAppOrigin(t, f)
	t.Cleanup(f.origin.Close)
}

func requireExpiryUploadSaved(t *testing.T, f *appFixture, id string, want bool) {
	t.Helper()
	var saved originState
	if err := load(context.Background(), f.originStore, "apps.origin", &saved); err != nil {
		t.Fatal(err)
	}
	if _, present := saved.Uploads[id]; present != want {
		t.Fatalf("durable retry record for %s present %t, want %t", id, present, want)
	}
}

type expiryStateStore struct {
	*memoryAppStore
	uploadID string
	saves    int
	check    func(context.Context) error
	checkErr error
	fail     error
	after    bool
}

func (s *expiryStateStore) SaveAppState(ctx context.Context, key string, data []byte) error {
	if key != "apps.origin" {
		return s.memoryAppStore.SaveAppState(ctx, key, data)
	}
	var state originState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode expiry fixture state: %w", err)
	}
	if _, exists := state.Uploads[s.uploadID]; exists {
		return s.memoryAppStore.SaveAppState(ctx, key, data)
	}
	s.saves++
	if s.check != nil {
		s.checkErr = s.check(ctx)
		if s.checkErr != nil {
			return s.checkErr
		}
	}
	if s.fail != nil && !s.after {
		return s.fail
	}
	if err := s.memoryAppStore.SaveAppState(ctx, key, data); err != nil {
		return err
	}
	return s.fail
}

func crashExpiryCandidate(t *testing.T, origin *Origin, request Request) {
	t.Helper()
	defer func() {
		failure := recover()
		cause, ok := failure.(error)
		if !ok || !errors.Is(cause, errSetupCrash) {
			t.Fatalf("setup did not reach its simulated crash: %v", failure)
		}
	}()
	_, _ = origin.Handle(context.Background(), request)
}
