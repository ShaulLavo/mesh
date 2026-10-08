package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAppShapedServiceHostIsNotIntercepted(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	now := time.Now()
	registry := testRegistry(t, ModeDirectTLS, now)
	defer registry.Close()
	origin, _ := testIdentity(t)
	if err := registry.Replace([]PublishedRoute{{Route: Route{PublicName: "docs.mesh.test", ServiceName: "docs"}, Origin: testResolvedOrigin(origin, testHTTPServerEndpoint(t, backend), now)}}); err != nil {
		t.Fatal(err)
	}
	registry.SetAppHandler(appHandlerFunc(func(w http.ResponseWriter, r *http.Request, name string) bool {
		http.Redirect(w, r, "https://apps.mesh.test/view?id=docs", http.StatusSeeOther)
		return true
	}))
	for _, path := range []string{"/docs/", "/missing"} {
		w := httptest.NewRecorder()
		registry.ServeHTTP(w, publicRequest(http.MethodGet, "docs.mesh.test", path))
		want := http.StatusNoContent
		if path == "/missing" {
			want = http.StatusNotFound
		}
		if w.Code != want {
			t.Fatalf("service path %s got %d", path, w.Code)
		}
	}
	unknown := httptest.NewRecorder()
	registry.ServeHTTP(unknown, publicRequest(http.MethodGet, "7k3d.mesh.test", "/"))
	if unknown.Code != http.StatusSeeOther {
		t.Fatalf("unknown app-shaped host got %d", unknown.Code)
	}
}

func TestAppShapedTunnelHostIsNotIntercepted(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	controller, registry, claimant := newProxyTunnel(t)
	activateProxyTunnel(t, controller, claimant, proxyEndpointFor(backend))
	calls := 0
	registry.SetAppHandler(appHandlerFunc(func(w http.ResponseWriter, r *http.Request, name string) bool {
		calls++
		http.NotFound(w, r)
		return true
	}))
	w := httptest.NewRecorder()
	registry.ServeHTTP(w, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if calls != 0 || w.Code != http.StatusNoContent {
		t.Fatalf("tunnel got %d, app calls=%d", w.Code, calls)
	}
}
