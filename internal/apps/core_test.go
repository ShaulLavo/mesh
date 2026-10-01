package apps

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryAppStore struct {
	mu       sync.Mutex
	values   map[string][]byte
	names    map[string]string
	inactive map[string]bool
}

func newMemoryAppStore() *memoryAppStore {
	return &memoryAppStore{values: map[string][]byte{}, names: map[string]string{}, inactive: map[string]bool{}}
}
func (s *memoryAppStore) LoadAppState(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.values[key]...), nil
}
func (s *memoryAppStore) SaveAppState(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = append([]byte(nil), value...)
	return nil
}
func (s *memoryAppStore) ReserveAppName(_ context.Context, name, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.names[name]; ok && existing != owner {
		return errors.New("collision")
	}
	s.names[name] = owner
	return nil
}
func (s *memoryAppStore) ReserveAppNameAndState(_ context.Context, name, owner, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.names[name]; ok && (existing != owner || s.inactive[name]) {
		return errors.New("collision")
	}
	s.names[name] = owner
	s.values[key] = append([]byte(nil), value...)
	return nil
}
func (s *memoryAppStore) AppNameExists(_ context.Context, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.names[name]
	return ok, nil
}
func (s *memoryAppStore) SetAppNameInactive(_ context.Context, name, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names[name] != owner {
		return errors.New("wrong owner")
	}
	s.inactive[name] = true
	return nil
}

type fakeWorkers struct {
	mu        sync.Mutex
	labels    map[string]string
	stopped   []string
	forgotten []string
}

func (w *fakeWorkers) Start(_ context.Context, label, command, root string, env []string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.labels == nil {
		w.labels = map[string]string{}
	}
	id := "worker-" + label
	w.labels[label] = id
	return id, nil
}
func (w *fakeWorkers) Stop(_ context.Context, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = append(w.stopped, id)
	for label, worker := range w.labels {
		if worker == id {
			delete(w.labels, label)
		}
	}
	return nil
}
func (w *fakeWorkers) Find(_ context.Context, label string) (string, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.labels[label]
	return id, ok, nil
}
func (w *fakeWorkers) Forget(_ context.Context, label string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.forgotten = append(w.forgotten, label)
	delete(w.labels, label)
}
func (w *fakeWorkers) Wait(context.Context, string) (int, error) { return 0, nil }

type appFixture struct {
	edge                        *Edge
	origin                      *Origin
	edgeStore, originStore      *memoryAppStore
	workers                     *fakeWorkers
	edgeKey, ownerKey, otherKey ed25519.PrivateKey
	now                         time.Time
	seq                         uint64
	root                        string
}

func identityFor(key ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}
func newAppFixture(t *testing.T) *appFixture {
	t.Helper()
	f := &appFixture{edgeStore: newMemoryAppStore(), originStore: newMemoryAppStore(), workers: &fakeWorkers{}, now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), root: t.TempDir()}
	for _, key := range []*ed25519.PrivateKey{&f.edgeKey, &f.ownerKey, &f.otherKey} {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		*key = private
	}
	f.edge = f.openEdge(t)
	origin, err := NewOrigin(context.Background(), OriginConfig{Store: f.originStore, Key: f.ownerKey, EdgeIdentity: identityFor(f.edgeKey), Exchange: f.edge.Exchange, Workers: f.workers, DataRoot: f.root, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	f.origin = origin
	t.Cleanup(origin.Close)
	return f
}
func (f *appFixture) openEdge(t *testing.T) *Edge {
	t.Helper()
	e, err := NewEdge(context.Background(), EdgeConfig{Store: f.edgeStore, Key: f.edgeKey, Allowed: map[string]bool{identityFor(f.ownerKey): true, identityFor(f.otherKey): true}, Resolve: func(context.Context, string) (netip.AddrPort, error) {
		return netip.MustParseAddrPort("127.0.0.1:9090"), nil
	}, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}
func (f *appFixture) signed(t *testing.T, q Request) Signed {
	t.Helper()
	f.seq++
	s, err := Sign("mesh-app/request/v1", identityFor(f.edgeKey), f.seq, q, f.ownerKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func (f *appFixture) exchange(t *testing.T, s Signed) (Result, error) {
	t.Helper()
	response, err := f.edge.Exchange(context.Background(), s)
	if err != nil {
		return Result{}, err
	}
	if err := response.Verify("mesh-app/response/v1", s.Owner, identityFor(f.edgeKey), f.now); err != nil {
		t.Fatal(err)
	}
	var reply edgeReply
	if err := json.Unmarshal(response.Body, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.RequestID != s.ID || response.Sequence != s.Sequence {
		t.Fatal("wrong acknowledgement")
	}
	if reply.Error != "" {
		return reply.Result, errors.New(reply.Error)
	}
	return reply.Result, nil
}
func (f *appFixture) operation(t *testing.T, q Request) Result {
	t.Helper()
	result, err := f.exchange(t, f.signed(t, q))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSignedAppAllocationSurvivesLostAcknowledgementAndRestart(t *testing.T) {
	f := newAppFixture(t)
	request := f.signed(t, Request{Action: "allocate", Kind: "static"})
	first, err := f.exchange(t, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.App == nil || first.App.Visibility != "private" || !first.App.ExpiresAt.Equal(f.now.Add(IdleTTL)) {
		t.Fatalf("unsafe initial app: %#v", first.App)
	}
	f.edge = f.openEdge(t)
	retry, err := f.exchange(t, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, retry) {
		t.Fatalf("retry allocated a new app: %#v versus %#v", first, retry)
	}
	listed := f.operation(t, Request{Action: "list"})
	if len(listed.Apps) != 1 {
		t.Fatalf("allocated %d apps", len(listed.Apps))
	}
	conflict, err := Sign("mesh-app/request/v1", identityFor(f.edgeKey), request.Sequence, Request{Action: "public", ID: first.App.ID}, f.ownerKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.exchange(t, conflict); err == nil {
		t.Fatal("accepted conflicting sequence")
	}
	if _, err := f.exchange(t, request); err == nil {
		t.Fatal("replayed old allocation after a newer operation")
	}
}

func TestAppTrafficAndExactDeadline(t *testing.T) {
	f := newAppFixture(t)
	app := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
	f.now = app.ExpiresAt.Add(-time.Nanosecond)
	observed, ok, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || !ok || !observed.ExpiresAt.Equal(app.ExpiresAt) {
		t.Fatalf("status polling refreshed TTL: %#v %v", observed, err)
	}
	touched, ok, err := f.edge.lookup(context.Background(), app.ID, true)
	if err != nil || !ok || !touched.ExpiresAt.Equal(f.now.Add(IdleTTL)) {
		t.Fatalf("traffic failed to refresh: %#v %v", touched, err)
	}
	f.edge = f.openEdge(t)
	f.now = touched.ExpiresAt
	expired, ok, err := f.edge.lookup(context.Background(), app.ID, true)
	if err != nil || !ok || expired.Status != "expired" || !expired.ExpiresAt.Equal(touched.ExpiresAt) {
		t.Fatalf("traffic revived expired app: %#v %v", expired, err)
	}
	for _, action := range []string{"renew", "public", "private", "activate"} {
		if _, err := f.exchange(t, f.signed(t, Request{Action: action, ID: app.ID})); err == nil {
			t.Fatalf("%s revived expired app", action)
		}
	}
	if err := f.edge.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	exists, err := f.edgeStore.AppNameExists(context.Background(), app.ID+"."+Domain)
	if err != nil || !exists {
		t.Fatal("expired URL became recyclable")
	}
}

func TestOtherAuthorizedHostCannotManagePublicOrPrivateApp(t *testing.T) {
	f := newAppFixture(t)
	app := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
	sequence := uint64(0)
	for _, visibility := range []string{"private", "public"} {
		f.operation(t, Request{Action: visibility, ID: app.ID})
		before, _, err := f.edge.lookup(context.Background(), app.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"public", "private", "renew", "delete", "cleanup", "activate", "inspect"} {
			sequence++
			request, err := Sign("mesh-app/request/v1", identityFor(f.edgeKey), sequence, Request{Action: action, ID: app.ID}, f.otherKey, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.exchange(t, request); err == nil {
				t.Fatalf("other host authorized %s for %s app", action, visibility)
			}
		}
		after, _, err := f.edge.lookup(context.Background(), app.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("unauthorized operations changed app")
		}
	}
}

func TestSignedOperationsRejectTamperingAndWrongEdge(t *testing.T) {
	f := newAppFixture(t)
	s := f.signed(t, Request{Action: "allocate", Kind: "static"})
	s.Body = json.RawMessage(`{"action":"allocate","kind":"server"}`)
	if _, err := f.edge.Exchange(context.Background(), s); err == nil {
		t.Fatal("accepted tampered request")
	}
	wrong, err := Sign("mesh-app/request/v1", identityFor(f.otherKey), 1, Request{Action: "allocate", Kind: "static"}, f.ownerKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.edge.Exchange(context.Background(), wrong); err == nil {
		t.Fatal("accepted operation signed for another edge")
	}
}

func TestDirectOriginNeverServesAppWithoutAdmission(t *testing.T) {
	f := newAppFixture(t)
	for _, proof := range []string{"", "not-base64", "e30"} {
		request := httptest.NewRequest(http.MethodGet, "http://origin/.mesh-app/origin/7k3d/index.html", nil)
		request.Header.Set("X-Mesh-App-Admission", proof)
		response := httptest.NewRecorder()
		if !f.origin.ServeHTTP(response, request) || response.Code != http.StatusNotFound {
			t.Fatalf("unadmitted origin request reached content: %d", response.Code)
		}
	}
}

func TestOriginPartitionStopsOwnedWorkersAtLeaseDeadline(t *testing.T) {
	f := newAppFixture(t)
	id := "7k3d"
	root := filepath.Join(f.root, "apps", id, "source")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	workerID, err := f.workers.Start(context.Background(), "app "+id, "server", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.origin.state.Apps[id] = localApp{Record: Record{ID: id, Owner: identityFor(f.ownerKey), Kind: "server", Status: "active", LeaseUntil: f.now.Add(LeaseTTL)}, Root: root, Command: "server", Session: workerID, Phase: "ready"}
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, errors.New("edge offline") }
	f.now = f.now.Add(LeaseTTL - time.Nanosecond)
	if err := f.origin.Sync(context.Background()); err == nil {
		t.Fatal("hid edge outage")
	}
	if len(f.workers.stopped) != 0 {
		t.Fatal("stopped process before lease expired")
	}
	f.now = f.now.Add(time.Nanosecond)
	if err := f.origin.Sync(context.Background()); err == nil {
		t.Fatal("hid edge outage")
	}
	if len(f.workers.stopped) != 1 || f.workers.stopped[0] != workerID {
		t.Fatal("left expired-lease process running")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("partition destroyed files before edge cleanup decision")
	}
}

func TestOwnerRenewalPreservesIdentityAndVisibility(t *testing.T) {
	f := newAppFixture(t)
	app := f.operation(t, Request{Action: "allocate", Kind: "static"}).App
	f.operation(t, Request{Action: "public", ID: app.ID})
	f.now = f.now.Add(time.Hour)
	renewed := f.operation(t, Request{Action: "renew", ID: app.ID}).App
	if renewed.ID != app.ID || renewed.Owner != app.Owner || renewed.Visibility != "public" || !renewed.ExpiresAt.Equal(f.now.Add(IdleTTL)) {
		t.Fatalf("renewed app %#v", renewed)
	}
}

func TestDecoderRejectsMultipleJSONValues(t *testing.T) {
	var q Request
	if err := decode([]byte(`{"action":"list"} {"action":"delete"}`), &q); err == nil {
		t.Fatal("accepted trailing operation")
	}
}

func uploadSource(t *testing.T, f *appFixture, source string) (string, string) {
	t.Helper()
	var archive bytes.Buffer
	digest, err := Pack(context.Background(), source, &archive)
	if err != nil {
		t.Fatal(err)
	}
	return uploadBytes(t, f, archive.Bytes()), digest
}
func uploadBytes(t *testing.T, f *appFixture, data []byte) string {
	t.Helper()
	created, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin"})
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(data); offset += ChunkSize {
		end := min(offset+ChunkSize, len(data))
		q := Request{Action: "upload.chunk", UploadID: created.UploadID, Offset: int64(offset), Data: data[offset:end]}
		if _, err := f.origin.Handle(context.Background(), q); err != nil {
			t.Fatal(err)
		}
		if _, err := f.origin.Handle(context.Background(), q); err != nil {
			t.Fatalf("exact chunk retry failed: %v", err)
		}
	}
	return created.UploadID
}
func sourceFixture(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "index.html"), []byte("original page"), 0600); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestExpiryDeletesManagedCopyAndLocalDataPreservingOtherAppAndSource(t *testing.T) {
	f := newAppFixture(t)
	source := sourceFixture(t)
	uploadID, digest := uploadSource(t, f, source)
	created, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: uploadID, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	app := created.App
	if app.Visibility != "private" || !app.Ready {
		t.Fatalf("created app not private/ready: %#v", app)
	}
	workspace := f.origin.state.Apps[app.ID].Root
	if workspace == source {
		t.Fatal("hosting caller directory directly")
	}
	if err := os.WriteFile(filepath.Join(workspace, "local.db"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	otherUpload, otherDigest := uploadSource(t, f, source)
	other, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: otherUpload, Digest: otherDigest})
	if err != nil {
		t.Fatal(err)
	}
	f.now = app.ExpiresAt.Add(-time.Hour)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "renew", ID: other.App.ID}); err != nil {
		t.Fatal(err)
	}
	f.now = app.ExpiresAt
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "apps", app.ID)); !os.IsNotExist(err) {
		t.Fatalf("expired managed files remain: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(source, "index.html")); err != nil || string(data) != "original page" { //nolint:gosec // Read the caller-owned fixture to verify expiry preserved it.
		t.Fatal("deleted caller source")
	}
	if _, err := os.Stat(f.origin.state.Apps[other.App.ID].Root); err != nil {
		t.Fatal("deleted another app")
	}
	final, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || final.Status != "expired" || final.Cleanup != "complete" {
		t.Fatalf("cleanup not reconciled: %#v %v", final, err)
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatalf("repeated cleanup failed: %v", err)
	}
}

func TestUploadRetriesRefuseConflictingBytesAndOffsets(t *testing.T) {
	f := newAppFixture(t)
	created, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin"})
	if err != nil {
		t.Fatal(err)
	}
	first := Request{Action: "upload.chunk", UploadID: created.UploadID, Data: []byte("first")}
	if _, err := f.origin.Handle(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := f.origin.Handle(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	for _, q := range []Request{{Action: "upload.chunk", UploadID: created.UploadID, Data: []byte("other")}, {Action: "upload.chunk", UploadID: created.UploadID, Offset: 10, Data: []byte("gap")}} {
		if _, err := f.origin.Handle(context.Background(), q); err == nil {
			t.Fatal("accepted conflicting upload retry")
		}
	}
}

func TestCreateCommitRetryDoesNotDuplicateApp(t *testing.T) {
	f := newAppFixture(t)
	uploadID, digest := uploadSource(t, f, sourceFixture(t))
	request := Request{Action: "create", Kind: "static", UploadID: uploadID, Digest: digest}
	first, err := f.origin.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.origin.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("lost commit acknowledgement retry failed: %v", err)
	}
	if second.App == nil || second.App.ID != first.App.ID {
		t.Fatal("commit retry duplicated app")
	}
}

func TestRejectedUpdatePreservesWorkingApp(t *testing.T) {
	f := newAppFixture(t)
	uploadID, digest := uploadSource(t, f, sourceFixture(t))
	created, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: uploadID, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	id := created.App.ID
	oldRoot := f.origin.state.Apps[id].Root
	nextUpload, nextDigest := uploadSource(t, f, sourceFixture(t))
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: id, Kind: "static", UploadID: nextUpload, Digest: nextDigest + "bad"}); err == nil {
		t.Fatal("accepted invalid update digest")
	}
	current, _, err := f.edge.lookup(context.Background(), id, false)
	if err != nil || current.Status != "active" || !current.Ready {
		t.Fatalf("rejected staged update removed working app: %#v %v", current, err)
	}
	if data, err := os.ReadFile(filepath.Join(oldRoot, "index.html")); err != nil || string(data) != "original page" { //nolint:gosec // Verify the previous fixture survived a rejected staged update.
		t.Fatal("rejected staged update destroyed active workspace")
	}
}

func TestHTTPPrivateGatePublicVisitorAndAnonymousManagement(t *testing.T) {
	f := newAppFixture(t)
	uploadID, digest := uploadSource(t, f, sourceFixture(t))
	created, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: uploadID, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	id := created.App.ID
	host := id + "." + Domain
	var forwarded atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		if !f.origin.ServeHTTP(w, r) {
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	endpoint, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.edge.config.Resolve = func(context.Context, string) (netip.AddrPort, error) { return netip.ParseAddrPort(endpoint.Host) }
	before := created.App.ExpiresAt
	f.now = f.now.Add(time.Hour)
	for _, path := range []string{"/", "/index.html", "/api/secret", "/.mesh-app/pill.js", "/.mesh-app/pill.css", "/socket"} {
		request := httptest.NewRequest(http.MethodGet, URL(id)+path, nil)
		if path == "/socket" {
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
		}
		result := httptest.NewRecorder()
		if !f.edge.ServeHost(result, request, host) {
			t.Fatalf("private %s not handled", path)
		}
		if path == "/socket" {
			if result.Code != http.StatusForbidden {
				t.Fatalf("private websocket status %d", result.Code)
			}
		} else if result.Code != http.StatusSeeOther || !strings.HasPrefix(result.Header().Get("Location"), ManagementOrigin+"/view") {
			t.Fatalf("private %s status %d", path, result.Code)
		}
		if strings.Contains(result.Body.String(), "original page") {
			t.Fatal("private gate exposed page")
		}
	}
	current, _, err := f.edge.lookup(context.Background(), id, false)
	if err != nil || !current.ExpiresAt.Equal(before) || forwarded.Load() != 0 {
		t.Fatalf("rejected visits touched origin/deadline: %#v, forwarded=%d", current, forwarded.Load())
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "public", ID: id}); err != nil {
		t.Fatal(err)
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := httptest.NewRecorder()
	if !f.edge.ServeHost(result, httptest.NewRequest(http.MethodGet, URL(id)+"/", nil), host) || result.Code != 200 || !strings.Contains(result.Body.String(), "original page") || !strings.Contains(result.Body.String(), "data-mesh-app") {
		t.Fatalf("public app unavailable: %d %s", result.Code, result.Body.String())
	}
	for _, action := range []string{"private", "public", "renew", "delete"} {
		body := url.Values{"id": {id}, "action": {action}, "csrf": {"forged"}}.Encode()
		request := httptest.NewRequest(http.MethodPost, ManagementOrigin+"/action", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", URL(id))
		result := httptest.NewRecorder()
		f.edge.ServeHost(result, request, ManagementHost)
		if result.Code != http.StatusUnauthorized && result.Code != http.StatusForbidden {
			t.Fatalf("anonymous %s status %d", action, result.Code)
		}
	}
	current, _, err = f.edge.lookup(context.Background(), id, false)
	if err != nil || current.Visibility != "public" || current.Status != "active" {
		t.Fatal("anonymous visitor mutated app")
	}
	result = httptest.NewRecorder()
	f.edge.ServeHost(result, httptest.NewRequest(http.MethodGet, ManagementOrigin+"/frame?id="+id, nil), ManagementHost)
	if result.Code != 200 || !strings.Contains(result.Body.String(), "Pair browser") || strings.Contains(result.Body.String(), "Make private") || strings.Contains(result.Body.String(), "action=delete") {
		t.Fatalf("visitor frame exposed owner controls: %d %s", result.Code, result.Body.String())
	}
}

func TestAmbiguousCreateAllocationRecoversAcrossOriginRestart(t *testing.T) {
	f := newAppFixture(t)
	upload, digest := uploadSource(t, f, sourceFixture(t))
	request := Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest}
	dropped := false
	f.origin.config.Exchange = func(ctx context.Context, s Signed) (Signed, error) {
		response, err := f.edge.Exchange(ctx, s)
		var q Request
		if err == nil && json.Unmarshal(s.Body, &q) == nil && q.Action == "allocate" && !dropped {
			dropped = true
			return Signed{}, errors.New("connection lost after edge commit")
		}
		return response, err
	}
	if _, err := f.origin.Handle(context.Background(), request); err == nil {
		t.Fatal("lost acknowledgement was hidden")
	}
	if !dropped {
		t.Fatal("did not exercise ambiguous allocation")
	}
	f.now = f.now.Add(5 * time.Minute)
	restarted, err := NewOrigin(context.Background(), OriginConfig{Store: f.originStore, Key: f.ownerKey, EdgeIdentity: identityFor(f.edgeKey), Exchange: f.edge.Exchange, Workers: f.workers, DataRoot: f.root, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	f.origin = restarted
	recovered, err := f.origin.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("pending signed mutation could not recover after freshness window: %v", err)
	}
	listed, err := f.origin.Handle(context.Background(), Request{Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Apps) != 1 || listed.Apps[0].ID != recovered.App.ID || !recovered.App.Ready {
		t.Fatalf("ambiguous retry duplicated app: %#v", listed.Apps)
	}
}
