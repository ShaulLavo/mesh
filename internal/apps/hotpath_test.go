package apps

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
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
	if len(f.edge.inflight) != 0 {
		t.Fatalf("inflight retains %d empty app maps", len(f.edge.inflight))
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
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	store := newMemoryAppStore()
	app := Record{ID: "7k3d", Owner: identityFor(key), Kind: "static", Status: "active", Visibility: "public", Ready: true, Generation: 1, ExpiresAt: now.Add(IdleTTL)}
	if err := save(context.Background(), store, "apps.edge", edgeState{Apps: map[string]Record{app.ID: app}, Owners: map[string]ownerState{}}); err != nil {
		b.Fatal(err)
	}
	edge, err := NewEdge(context.Background(), EdgeConfig{Store: store, Key: key, Now: func() time.Time { return now }, Resolve: func(context.Context, string) (netip.AddrPort, error) {
		return netip.MustParseAddrPort("127.0.0.1:9090"), nil
	}})
	if err != nil {
		b.Fatal(err)
	}
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
