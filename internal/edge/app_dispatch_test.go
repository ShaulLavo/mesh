package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFourCharacterServiceHostKeepsOrdinaryRouting(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	now := time.Now()
	registry := testRegistry(t, ModeDirectTLS, now)
	defer registry.Close()
	origin, _ := testIdentity(t)
	if err := registry.Replace([]PublishedRoute{{Route: Route{PublicName: "docs.mesh.test", ServiceName: "docs"}, Origin: testResolvedOrigin(origin, testHTTPServerEndpoint(t, backend), now)}}); err != nil {
		t.Fatal(err)
	}
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
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown app-shaped host got %d", unknown.Code)
	}
}

func TestFourCharacterTunnelHostKeepsOrdinaryRouting(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	controller, registry, claimant := newProxyTunnel(t)
	activateProxyTunnel(t, controller, claimant, proxyEndpointFor(backend))
	w := httptest.NewRecorder()
	registry.ServeHTTP(w, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("tunnel got %d", w.Code)
	}
}

func TestPublicRegistryDoesNotServeTemporaryAppHosts(t *testing.T) {
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	for _, host := range []string{"7k3d.mesh.test", "apps.mesh.test"} {
		assertPublicRegistryUnknownHost(t, registry, host)
	}
}

func assertPublicRegistryUnknownHost(t *testing.T, registry *Registry, host string) {
	t.Helper()
	for _, path := range []string{"/", "/frame?id=7k3d", "/mesh", "/m%65sh/control"} {
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, publicRequest(http.MethodGet, host, path))
		if response.Code != http.StatusNotFound || response.Header().Get("Location") != "" {
			t.Fatalf("public temporary-app host %s%s got %d, redirect=%q", host, path, response.Code, response.Header().Get("Location"))
		}
	}
}
