package apps

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/webauth"
)

type activityCountingStore struct {
	*memoryAppStore
	writes atomic.Int64
}

func (s *activityCountingStore) SaveAppState(ctx context.Context, key string, value []byte) error {
	if key == "apps.edge" {
		s.writes.Add(1)
	}
	return s.memoryAppStore.SaveAppState(ctx, key, value)
}
func countedActivity(t *testing.T, f *appFixture) *activityCountingStore {
	t.Helper()
	s := &activityCountingStore{memoryAppStore: f.edgeStore}
	f.edge.config.Store = s
	return s
}
func publicStaticApp(t *testing.T, f *appFixture) Record {
	t.Helper()
	app := createStaticApp(t, f)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "public", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	return app
}
func TestAdmissionActivityWriteBudget(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	networkOrigin(t, f)
	s := countedActivity(t, f)
	for range 50 {
		f.now = f.now.Add(time.Second)
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, httptest.NewRequest(http.MethodGet, URL(app.ID)+"/index.html", nil), app.ID+"."+Domain)
		if response.Code != http.StatusOK {
			t.Fatalf("status %d: %s", response.Code, response.Body.String())
		}
	}
	t.Logf("50 admissions and response bodies within one minute: %d edge writes", s.writes.Load())
	if s.writes.Load() > 1 {
		t.Errorf("edge writes = %d, want at most 1", s.writes.Load())
	}
}
func TestWebSocketAdmissionWithoutActivityDoesNotWrite(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	s := countedActivity(t, f)
	r := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
	r.Header.Set("Upgrade", "websocket")
	_, _, release, err := f.edge.admit(r, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if s.writes.Load() != 0 {
		t.Fatalf("idle websocket admission writes = %d, want 0", s.writes.Load())
	}
}
func TestActivityRestartDeadlineSlack(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	s := countedActivity(t, f)
	for range 179 {
		f.now = f.now.Add(time.Second)
		_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	current, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	restarted := f.openEdge(t)
	durable, _, err := restarted.lookup(context.Background(), app.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	lag := current.ExpiresAt.Sub(durable.ExpiresAt)
	if lag < 0 || lag > time.Minute {
		t.Fatalf("restart lost %s of activity", lag)
	}
	if !current.ExpiresAt.Equal(f.now.Add(IdleTTL)) {
		t.Fatal("in-memory deadline stopped advancing")
	}
	t.Logf("179 one-second admissions: writes=%d, restart deadline lag=%s", s.writes.Load(), lag)
	if s.writes.Load() > 3 {
		t.Errorf("edge writes = %d, want at most 3", s.writes.Load())
	}
}
func viewCookie(t *testing.T, f *appFixture, owner *http.Cookie, id string) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/view?id="+id, nil)
	r.AddCookie(owner)
	ticket, err := f.edge.auth.IssueView(context.Background(), r, identityFor(f.ownerKey), id)
	if err != nil {
		t.Fatal(err)
	}
	consume := httptest.NewRecorder()
	f.edge.ServeHost(consume, httptest.NewRequest(http.MethodGet, URL(id)+"/?mesh_view="+ticket, nil), id+"."+Domain)
	return cookieNamed(t, consume, webauth.ViewCookie)
}
func TestBrowserRevocationIsolatesInflight(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	first, second := pairedOwner(t, f), pairedOwner(t, f)
	firstView, secondView := viewCookie(t, f, first, app.ID), viewCookie(t, f, second, app.ID)
	admit := func(cookie *http.Cookie) *http.Request {
		r := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		_, admitted, release, err := f.edge.admit(r, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		return admitted
	}
	revoked, other, public := admit(firstView), admit(secondView), admit(nil)
	r := httptest.NewRequest(http.MethodGet, ManagementOrigin, nil)
	r.AddCookie(first)
	browser, err := f.edge.auth.Browser(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "browser.revoke", BrowserID: browser.ID}); err != nil {
		t.Fatal(err)
	}
	if revoked.Context().Err() == nil {
		t.Error("revoked browser request was not canceled")
	}
	if other.Context().Err() != nil {
		t.Error("another browser request was canceled")
	}
	if public.Context().Err() != nil {
		t.Error("public visitor request was canceled")
	}
}
func TestInflightReleaseDeletesEmptyEntries(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	for range 20 {
		_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	rt := (*f.edge.runtime.Load())[app.ID]
	if rt.inflight != nil {
		t.Fatalf("inflight retains an empty app map")
	}
}
func countingHTTPServer(t *testing.T, handler http.Handler) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	count := &atomic.Int64{}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			count.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	return server, count
}
func TestProxyConnectionsAreReused(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	upstream, upstreamConns := countingHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "upstream") }))
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	port := netip.MustParseAddrPort(upstreamURL.Host).Port()
	local := f.origin.state.Apps[app.ID]
	local.Record.Kind, local.Port = "server", int(port)
	f.origin.state.Apps[app.ID] = local
	f.origin.serving[app.ID] = serving{upstream: netip.MustParseAddrPort(upstreamURL.Host)}
	f.origin.publishRoutes()
	origin, originConns := countingHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.origin.ServeHTTP(w, r) }))
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := netip.MustParseAddrPort(originURL.Host)
	f.edge.config.Resolve = func(context.Context, string) (netip.AddrPort, error) { return endpoint, nil }
	for range 20 {
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, httptest.NewRequest(http.MethodGet, URL(app.ID)+"/asset", nil), app.ID+"."+Domain)
		if response.Code != http.StatusOK || response.Body.String() != "upstream" {
			t.Fatalf("proxy response = %d %q", response.Code, response.Body.String())
		}
	}
	t.Logf("20 requests: edge-origin connections=%d, origin-upstream connections=%d", originConns.Load(), upstreamConns.Load())
	if originConns.Load() > 1 {
		t.Errorf("edge-origin connections = %d, want 1", originConns.Load())
	}
	if upstreamConns.Load() > 1 {
		t.Errorf("origin-upstream connections = %d, want 1", upstreamConns.Load())
	}
}
func BenchmarkAppAdmission(b *testing.B) {
	for _, tombstones := range []int{0, 1000, 16000} {
		b.Run(fmt.Sprintf("tombstones=%d", tombstones), func(b *testing.B) { benchmarkAppAdmission(b, tombstones) })
	}
}
func benchmarkAppAdmission(b *testing.B, tombstones int) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	store := newMemoryAppStore()
	app := Record{ID: "7k3d", Owner: identityFor(key), Kind: "static", Status: "active", Visibility: "public", Ready: true, Generation: 1, ExpiresAt: now.Add(IdleTTL)}
	apps := map[string]Record{app.ID: app}
	for n := range tombstones {
		id := fmt.Sprintf("gone-%d", n)
		apps[id] = Record{ID: id, Status: "deleted", Cleanup: "complete"}
	}
	if err := save(context.Background(), store, "apps.edge", edgeState{Apps: apps, Owners: map[string]ownerState{}}); err != nil {
		b.Fatal(err)
	}
	edge, err := NewEdge(context.Background(), EdgeConfig{Store: store, Key: key, Now: func() time.Time { return now }, Resolve: func(context.Context, string) (netip.AddrPort, error) {
		return netip.MustParseAddrPort("127.0.0.1:9090"), nil
	}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(edge.Close)
	s := &activityCountingStore{memoryAppStore: store}
	edge.config.Store = s
	r := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, release, err := edge.admit(r, app.ID)
		if err != nil {
			b.Fatal(err)
		}
		release()
	}
	b.ReportMetric(float64(s.writes.Load())/float64(b.N), "writes/op")
}

func TestWarmAdmissionDoesNotTakeEdgeMutationLock(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	store := countedActivity(t, f)
	f.edge.mu.Lock()
	defer f.edge.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID)
		if release != nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("warm admission waited for the edge mutation mutex")
	}
	if store.writes.Load() != 0 {
		t.Fatal("warm admission wrote edge state")
	}
}
func TestConcurrentActivityCoalescesDeadlineWrites(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	store := countedActivity(t, f)
	f.now = f.now.Add(time.Minute)
	var requests sync.WaitGroup
	errors := make(chan error, 32)
	for range 32 {
		requests.Go(func() {
			_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID)
			if release != nil {
				release()
			}
			errors <- err
		})
	}
	requests.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if store.writes.Load() != 1 {
		t.Fatalf("simultaneous deadline crossings wrote %d times, want 1", store.writes.Load())
	}
	restarted := f.openEdge(t)
	current, _, err := restarted.lookup(context.Background(), app.ID, false)
	if err != nil || !current.ExpiresAt.Equal(f.now.Add(IdleTTL)) {
		t.Fatalf("coalesced deadline did not survive restart: %v", err)
	}
}
func TestRevokedViewIsRecheckedAfterOriginResolution(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	forwarded := networkOrigin(t, f)
	owner := pairedOwner(t, f)
	view := viewCookie(t, f, owner, app.ID)
	resolve := f.edge.config.Resolve
	entered, continueResolve := make(chan struct{}), make(chan struct{})
	f.edge.config.Resolve = func(ctx context.Context, owner string) (netip.AddrPort, error) {
		close(entered)
		<-continueResolve
		return resolve(ctx, owner)
	}
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
		r.AddCookie(view)
		f.edge.ServeHost(response, r, app.ID+"."+Domain)
	}()
	<-entered
	r := httptest.NewRequest(http.MethodGet, ManagementOrigin, nil)
	r.AddCookie(owner)
	browser, err := f.edge.auth.Browser(context.Background(), r)
	if err != nil {
		close(continueResolve)
		<-done
		t.Fatal(err)
	}
	_, err = f.origin.Handle(context.Background(), Request{Action: "browser.revoke", BrowserID: browser.ID})
	close(continueResolve)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusForbidden || forwarded.Load() != 0 {
		t.Fatalf("revoked cached view reached origin: status=%d forwarded=%d", response.Code, forwarded.Load())
	}
}
func TestStaleStreamActivityDoesNotRefreshReplacement(t *testing.T) {
	f := newAppFixture(t)
	app := publicStaticApp(t, f)
	f.seq = f.edge.state.Owners[identityFor(f.ownerKey)].Sequence
	f.operation(t, Request{Action: "activate", ID: app.ID, UploadID: "replacement"})
	before, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	if _, _, err := f.edge.activity(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	after, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("old stream refreshed the new generation")
	}
}
func TestRouteHandlersFollowRevisionNotLease(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	before := (*f.origin.routes.Load())[app.ID]
	f.origin.publishRoutes()
	same := (*f.origin.routes.Load())[app.ID]
	if before.Handler == nil || before.Handler != same.Handler {
		t.Fatal("static handler rebuilt without a revision change")
	}
	changed := f.origin.state.Apps[app.ID]
	changed.Record.Revision = "replacement"
	f.origin.state.Apps[app.ID] = changed
	f.origin.publishRoutes()
	after := (*f.origin.routes.Load())[app.ID]
	if after.Handler == before.Handler {
		t.Fatal("static handler reused across revisions")
	}
}

type pausedActivityStore struct {
	*memoryAppStore
	paused           atomic.Bool
	entered, release chan struct{}
}

func (s *pausedActivityStore) SaveAppState(ctx context.Context, key string, value []byte) error {
	if key == "apps.edge" && s.paused.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.memoryAppStore.SaveAppState(ctx, key, value)
}
func TestDeadlineFlushDoesNotBlockOrEraseOtherAppActivity(t *testing.T) {
	f := newAppFixture(t)
	first := publicStaticApp(t, f)
	f.now = f.now.Add(time.Minute)
	second := publicStaticApp(t, f)
	var clock atomic.Int64
	clock.Store(f.now.UnixNano())
	f.edge.config.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	store := &pausedActivityStore{memoryAppStore: f.edgeStore, entered: make(chan struct{}), release: make(chan struct{})}
	f.edge.config.Store = store
	var once sync.Once
	unblock := func() { once.Do(func() { close(store.release) }) }
	defer unblock()
	admit := func(id string) error {
		_, _, release, err := f.edge.admit(httptest.NewRequest(http.MethodGet, URL(id), nil), id)
		if release != nil {
			release()
		}
		return err
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- admit(first.ID) }()
	<-store.entered
	clock.Add(int64(time.Second))
	secondDone := make(chan error, 1)
	go func() { secondDone <- admit(second.ID) }()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("another app's snapshot write blocked warm admission")
	}
	unblock()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	current, _, err := f.edge.lookup(context.Background(), second.ID, false)
	if err != nil || !current.ExpiresAt.Equal(time.Unix(0, clock.Load()).Add(IdleTTL)) {
		t.Fatalf("publishing an older snapshot erased newer app activity: %v", err)
	}
}
