package serve

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func mustIsolatedProxyHandler(t *testing.T, backendURL, prefix string) http.Handler {
	t.Helper()
	parsed, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(Service{Name: "api", Kind: Proxy, Target: port, Isolate: true}, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestProxyRedirectsBareMountToTrailingSlash(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("backend received %s, want a redirect before the proxy", r.URL.Path)
	}))
	defer backend.Close()

	handler := mustProxyHandler(t, backend.URL, "/api")
	for path, want := range map[string]string{
		"/api":     "/api/",
		"/api?x=1": "/api/?x=1",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusPermanentRedirect || recorder.Header().Get("Location") != want {
			t.Fatalf("%s: status %d location %q, want 308 to %q", path, recorder.Code, recorder.Header().Get("Location"), want)
		}
	}
}

func TestProxyStillForwardsPathsUnderTheMount(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("backend path = %q, want /", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	handler := mustProxyHandler(t, backend.URL, "/api")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 from the backend", recorder.Code)
	}
}

func TestIsolatedServicesSendCrossOriginHeaders(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The backend tries to weaken the policy; the proxy must win.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cross-Origin-Embedder-Policy", "unsafe-none")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	staticHandler, err := Handler(Service{Name: "site", Kind: Static, Target: root, Isolate: true}, "/site")
	if err != nil {
		t.Fatal(err)
	}
	proxyHandler := mustIsolatedProxyHandler(t, backend.URL, "/api")

	for name, probe := range map[string]struct {
		handler http.Handler
		path    string
	}{
		"static": {staticHandler, "/site/"},
		"proxy":  {proxyHandler, "/api/"},
	} {
		recorder := httptest.NewRecorder()
		probe.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, probe.path, nil))
		header := recorder.Header()
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", name, recorder.Code)
		}
		if got := header.Values("Cross-Origin-Opener-Policy"); len(got) != 1 || got[0] != "same-origin" {
			t.Fatalf("%s: opener policy = %v", name, got)
		}
		if got := header.Values("Cross-Origin-Embedder-Policy"); len(got) != 1 || got[0] != "require-corp" {
			t.Fatalf("%s: embedder policy = %v", name, got)
		}
	}

	plain, err := Handler(Service{Name: "site", Kind: Static, Target: root}, "/site")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	plain.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/site/", nil))
	if recorder.Header().Get("Cross-Origin-Opener-Policy") != "" || recorder.Header().Get("Cross-Origin-Embedder-Policy") != "" {
		t.Fatalf("service without --isolate sent isolation headers: %v", recorder.Header())
	}
}
