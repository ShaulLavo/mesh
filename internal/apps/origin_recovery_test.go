package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type reviewBlockingWorkers struct {
	*fakeWorkers
	entered, release chan struct{}
}

func (w *reviewBlockingWorkers) Wait(context.Context, string) (int, error) {
	close(w.entered)
	<-w.release
	return 0, nil
}

func TestSetupDoesNotBlockOtherApps(t *testing.T) {
	f := newAppFixture(t)
	first := createStaticApp(t, f)
	workers := &reviewBlockingWorkers{fakeWorkers: f.workers, entered: make(chan struct{}), release: make(chan struct{})}
	f.origin.config.Workers = workers
	upload, digest := uploadSource(t, f, sourceFixture(t))
	mutationDone := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", Setup: "long setup", UploadID: upload, Digest: digest})
		mutationDone <- err
	}()
	<-workers.entered
	proof, err := Sign("mesh-app/admission/v1", identityFor(f.ownerKey), 1, admission{ID: first.ID, Generation: first.Generation, Method: http.MethodGet, URI: "/", Host: first.ID + "." + Domain, Until: f.now.Add(30 * time.Second)}, f.edgeKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(proof)
	req := httptest.NewRequest(http.MethodGet, "http://origin/.mesh-app/origin/"+first.ID+"/", nil)
	req.Header.Set("X-Mesh-App-Admission", base64.RawURLEncoding.EncodeToString(raw))
	requestDone := make(chan int, 1)
	go func() { res := httptest.NewRecorder(); f.origin.ServeHTTP(res, req); requestDone <- res.Code }()
	blocked := false
	status := 0
	select {
	case status = <-requestDone:
	case <-time.After(time.Second):
		blocked = true
	}
	close(workers.release)
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if blocked {
		<-requestDone
		t.Fatal("existing app request blocked behind another app's setup")
	}
	if status != http.StatusOK {
		t.Fatalf("existing app returned %d during setup", status)
	}
}

func TestActivationLostAcknowledgementRetry(t *testing.T) {
	cases := []struct {
		name, action string
		restart      bool
	}{
		{"create/restart-sync", "create", true},
		{"create/direct-retry", "create", false},
		{"update/restart-sync", "update", true},
		{"update/direct-retry", "update", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testActivationRetry(t, tc.action, tc.restart)
		})
	}
}

func testActivationRetry(t *testing.T, action string, restart bool) {
	t.Helper()
	f := newAppFixture(t)
	q := Request{Action: action}
	if action == "update" {
		q.ID = createStaticApp(t, f).ID
	}
	upload, digest := uploadSource(t, f, sourceFixture(t))
	q.UploadID, q.Digest = upload, digest
	var committed string
	f.origin.config.Exchange = func(ctx context.Context, s Signed) (Signed, error) {
		response, err := f.edge.Exchange(ctx, s)
		var request Request
		_ = json.Unmarshal(s.Body, &request)
		if err == nil && request.Action == "activate" && committed == "" {
			committed = request.ID
			return Signed{}, errors.New("lost activation ack")
		}
		return response, err
	}
	if _, err := f.origin.Handle(context.Background(), q); err == nil {
		t.Fatal("fault not triggered")
	}
	if restart {
		restartAppOrigin(t, f)
		if err := f.origin.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	retry, err := f.origin.Handle(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if retry.App.ID != committed {
		t.Fatalf("retried create duplicated committed app: original %s, retry %s", committed, retry.App.ID)
	}
	if len(f.origin.state.Apps) != 1 || f.origin.state.Apps[committed].Commit != nil {
		t.Fatal("activation left duplicate apps or incomplete commit")
	}
	if _, ok := f.origin.state.Uploads[upload]; ok {
		t.Fatal("activation did not consume upload")
	}
	if _, err := os.Stat(f.origin.uploadPath(upload)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed archive retained: %v", err)
	}
	restartAppOrigin(t, f)
	repeated, err := f.origin.Handle(context.Background(), q)
	if err != nil || repeated.App.ID != committed || repeated.App.Generation != retry.App.Generation {
		t.Fatalf("receipt did not survive restart: %#v %v", repeated, err)
	}
	q.Setup = "changed recipe"
	if _, err := f.origin.Handle(context.Background(), q); err == nil {
		t.Fatal("retry accepted conflicting recipe")
	}
}

func restartAppOrigin(t *testing.T, f *appFixture) {
	t.Helper()
	restarted, err := NewOrigin(context.Background(), OriginConfig{Store: f.originStore, Key: f.ownerKey, EdgeIdentity: identityFor(f.edgeKey), Exchange: f.edge.Exchange, Workers: f.workers, DataRoot: f.root, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	f.origin = restarted
}

type appPhaseFaultStore struct {
	*memoryAppStore
	fault  bool
	phase  string
	before bool
}

func (s *appPhaseFaultStore) SaveAppState(ctx context.Context, key string, data []byte) error {
	fault := s.shouldFail(key, data)
	if fault && s.before {
		return errors.New("failed persistence")
	}
	if err := s.memoryAppStore.SaveAppState(ctx, key, data); err != nil {
		return err
	}
	if fault {
		return errors.New("lost persistence acknowledgement")
	}
	return nil
}
func (s *appPhaseFaultStore) shouldFail(key string, data []byte) bool {
	if key != "apps.origin" || !s.fault {
		return false
	}
	var state originState
	_ = json.Unmarshal(data, &state)
	for _, a := range state.Apps {
		if a.Phase == s.phase && a.Commit != nil {
			s.fault = false
			return true
		}
	}
	return false
}
func TestInterruptedUpdatePreservesNewRevision(t *testing.T) {
	f := newAppFixture(t)
	original := createStaticApp(t, f)
	oldRoot := f.origin.state.Apps[original.ID].Root
	if _, err := f.origin.Handle(context.Background(), Request{Action: "public", ID: original.ID}); err != nil {
		t.Fatal(err)
	}
	_, inflight, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(original.ID), nil), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.now = f.now.Add(time.Hour)
	upload, digest := uploadSource(t, f, sourceFixture(t))
	f.origin.config.Store = &appPhaseFaultStore{memoryAppStore: f.originStore, fault: true, phase: "activating"}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: original.ID, Kind: "static", UploadID: upload, Digest: digest}); err == nil {
		t.Fatal("fault not triggered")
	}
	restartAppOrigin(t, f)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	app, _, err := f.edge.lookup(context.Background(), original.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if app.Revision != upload || app.Generation != original.Generation+1 {
		t.Fatalf("recovered update uses stale revision/generation: revision %s want %s, generation %d want %d", app.Revision, upload, app.Generation, original.Generation+1)
	}
	if !app.ExpiresAt.Equal(f.now.Add(IdleTTL)) || inflight.Context().Err() == nil {
		t.Fatal("recovered update did not renew expiry and cancel previous streams")
	}
	if _, err := os.Stat(oldRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery retained old workspace: %v", err)
	}
}

func TestActivationReceiptSaveRecovery(t *testing.T) {
	for _, before := range []bool{true, false} {
		name := "lost-acknowledgement"
		if before {
			name = "failed-write"
		}
		t.Run(name, func(t *testing.T) { testReceiptSaveRecovery(t, before) })
	}
}

func testReceiptSaveRecovery(t *testing.T, before bool) {
	t.Helper()
	f := newAppFixture(t)
	original := createStaticApp(t, f)
	oldRoot := f.origin.state.Apps[original.ID].Root
	upload, digest := uploadSource(t, f, sourceFixture(t))
	q := Request{Action: "update", ID: original.ID, UploadID: upload, Digest: digest}
	f.origin.config.Store = &appPhaseFaultStore{memoryAppStore: f.originStore, fault: true, phase: "ready", before: before}
	if _, err := f.origin.Handle(context.Background(), q); err == nil {
		t.Fatal("fault not triggered")
	}
	restartAppOrigin(t, f)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	retried, err := f.origin.Handle(context.Background(), q)
	if err != nil || retried.App.ID != original.ID || retried.App.Generation != original.Generation+1 {
		t.Fatalf("receipt save recovery duplicated update: %#v %v", retried, err)
	}
	if len(f.origin.state.Apps) != 1 || f.origin.state.Apps[original.ID].Commit != nil {
		t.Fatal("receipt save recovery left an incomplete commit")
	}
	if _, err := os.Stat(oldRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt save recovery retained old workspace: %v", err)
	}
}
