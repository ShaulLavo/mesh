package serve

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestStaticHandlerServesFilesAndIndexesUnderPrefix(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("home"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "asset.txt"), []byte("asset"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(Service{Name: "site", Kind: Static, Target: root}, "/site")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/site/", body: "home"},
		{path: "/site/asset.txt", body: "asset"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK || response.Body.String() != test.body {
			t.Fatalf("GET %s = %d %q, want 200 %q", test.path, response.Code, response.Body.String(), test.body)
		}
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sitewide/asset.txt", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("prefix lookalike status = %d, want 404", response.Code)
	}
}

func TestDirectoryHandlersFailClosedForTraversal(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(outside, []byte("do not serve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "secret-link")); err != nil {
		t.Fatal(err)
	}

	attacks := []string{
		"/prefix/../../secret.txt",
		"/prefix/%2e%2e/%2e%2e/secret.txt",
		"/prefix/%252e%252e/%252e%252e/secret.txt",
		"/prefix/secret-link",
		"/prefix/file%00.txt",
		"/prefix/file%2500.txt",
	}
	for _, kind := range []Kind{Static, Files} {
		handler, err := Handler(Service{Name: "prefix", Kind: kind, Target: root}, "/prefix")
		if err != nil {
			t.Fatal(err)
		}
		for _, attack := range attacks {
			t.Run(string(kind)+" "+attack, func(t *testing.T) {
				request := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: attack, RawPath: attack}, Header: make(http.Header)}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusBadRequest && response.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 400 or 404", response.Code)
				}
				if strings.Contains(response.Body.String(), "do not serve") {
					t.Fatal("response leaked the outside file")
				}
			})
		}
	}
}

func TestMissingDirectoryIsRegisteredAsUnhealthy(t *testing.T) {
	service := Service{Name: "gone", Kind: Static, Target: filepath.Join(t.TempDir(), "gone")}
	registry, err := NewRegistry([]Service{service})
	if err != nil {
		t.Fatal(err)
	}
	statuses := registry.Status()
	if len(statuses) != 1 || statuses[0].Healthy || !strings.Contains(statuses[0].Problem, "unavailable") {
		t.Fatalf("status = %#v, want one unhealthy service", statuses)
	}

	response := httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/gone/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing-root status = %d, want 503", response.Code)
	}
}

func TestFilesHandlerBuildsWorkingLinksAtRootAndNestedPrefixes(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello world.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, prefix := range []string{"/files", "/"} {
		t.Run(prefix, func(t *testing.T) {
			handler, err := Handler(Service{Name: "files", Kind: Files, Target: root}, prefix)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, prefixWithSlash(prefix), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("listing status = %d: %s", response.Code, response.Body.String())
			}
			wantLink := joinURLPrefix(prefix, "hello%20world.txt")
			if !strings.Contains(response.Body.String(), `href="`+wantLink+`"`) {
				t.Fatalf("listing does not contain working link %q: %s", wantLink, response.Body.String())
			}

			download := httptest.NewRecorder()
			handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, wantLink, nil))
			if download.Code != http.StatusOK || download.Body.String() != "hello" {
				t.Fatalf("download = %d %q, want 200 hello", download.Code, download.Body.String())
			}
		})
	}
}

func TestProxyForwardsPathHeadersAndStreams(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" || r.URL.RawQuery != "value=one" {
			t.Errorf("backend URL = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("X-Forwarded-Proto") != "http" || r.Header.Get("X-Forwarded-Prefix") != "/api" || r.Header.Get("X-Forwarded-For") == "" {
			t.Errorf("forwarded headers = %#v", r.Header)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("backend response does not support flushing")
			return
		}
		_, _ = io.WriteString(w, "first\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "second\n")
	}))
	defer backend.Close()

	handler := mustProxyHandler(t, backend.URL, "/api")
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	response, err := http.Get(proxy.URL + "/api/events?value=one")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck // test cleanup
	firstLine := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(response.Body).ReadString('\n')
		firstLine <- line
	}()
	select {
	case got := <-firstLine:
		if got != "first\n" {
			t.Fatalf("first streamed line = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy buffered the flushed response")
	}
	close(release)
}

func TestProxyPreservesForwardingMetadataOnlyFromPinnedPeer(t *testing.T) {
	observed := make(chan http.Header, 3)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		observed <- request.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	parsed, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	pinned := netip.MustParseAddr("100.64.0.2")
	registry, err := NewRegistryWithReservedPrefix(
		[]Service{{Name: "api", Kind: Proxy, Target: port}},
		ReservedPrefix,
		func(address netip.Addr) bool { return address == pinned },
	)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://app.shaulavo.dev/api/value", nil)
	request.RemoteAddr = "100.64.0.2:43120"
	request.Header.Set("X-Forwarded-For", "203.0.113.77")
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("trusted edge response = %d", response.Code)
	}
	trusted := <-observed
	if trusted.Get("X-Forwarded-For") != "203.0.113.77" || trusted.Get("X-Forwarded-Proto") != "https" ||
		trusted.Get("X-Forwarded-Host") != "app.shaulavo.dev" || trusted.Get("X-Forwarded-Prefix") != "/api" {
		t.Fatalf("trusted forwarding metadata = %#v", trusted)
	}

	request = httptest.NewRequest(http.MethodGet, "http://app.shaulavo.dev/api/value", nil)
	request.RemoteAddr = "100.64.0.3:43120"
	request.Header.Set("Forwarded", "for=attacker")
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Real-IP", "198.51.100.8")
	response = httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	untrusted := <-observed
	if untrusted.Get("X-Forwarded-For") != "100.64.0.3" || untrusted.Get("X-Forwarded-Proto") != "http" ||
		untrusted.Get("Forwarded") != "" || untrusted.Get("X-Real-IP") != "" {
		t.Fatalf("untrusted forwarding metadata = %#v", untrusted)
	}

	request = httptest.NewRequest(http.MethodGet, "http://app.shaulavo.dev/api/value", nil)
	request.RemoteAddr = "100.64.0.2:43120"
	request.Header.Add("X-Forwarded-For", "203.0.113.77")
	request.Header.Add("X-Forwarded-For", "198.51.100.8")
	request.Header.Set("X-Forwarded-Proto", "https")
	response = httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	malformed := <-observed
	if malformed.Get("X-Forwarded-For") != "100.64.0.2" || malformed.Get("X-Forwarded-Proto") != "http" {
		t.Fatalf("malformed trusted metadata was preserved: %#v", malformed)
	}
}

func TestProxyPassesWebSocketUpgrade(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket" {
			t.Errorf("backend WebSocket path = %q, want /socket", r.URL.Path)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept WebSocket: %v", err)
			return
		}
		defer conn.CloseNow() //nolint:errcheck // test cleanup
		messageType, payload, err := conn.Read(r.Context())
		if err != nil {
			t.Errorf("read WebSocket: %v", err)
			return
		}
		if err := conn.Write(r.Context(), messageType, append([]byte("echo:"), payload...)); err != nil {
			t.Errorf("write WebSocket: %v", err)
		}
	}))
	defer backend.Close()

	proxy := httptest.NewServer(mustProxyHandler(t, backend.URL, "/api"))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/api/socket"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow() //nolint:errcheck // test cleanup
	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText || string(payload) != "echo:hello" {
		t.Fatalf("WebSocket response = %v %q", messageType, payload)
	}
}

func TestRegistryUsesLongestRouteAndReservesProtocolPrefix(t *testing.T) {
	outer := t.TempDir()
	inner := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "value"), []byte("outer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "value"), []byte("inner"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry([]Service{
		{Name: "api", Kind: Static, Target: outer},
		{Name: "api/v2", Kind: Static, Target: inner},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/value", nil))
	if response.Code != http.StatusOK || response.Body.String() != "inner" {
		t.Fatalf("longest route response = %d %q", response.Code, response.Body.String())
	}

	if _, err := NewRegistry([]Service{{Name: "mesh/site", Kind: Static, Target: outer}}); err == nil {
		t.Fatal("service under the reserved protocol prefix succeeded")
	}
	custom, err := NewRegistryWithReservedPrefix([]Service{{Name: "mesh/site", Kind: Static, Target: outer}}, "/control/ws", nil)
	if err != nil {
		t.Fatalf("service outside the configured protocol prefix: %v", err)
	}
	response = httptest.NewRecorder()
	custom.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/mesh/site/value", nil))
	if response.Code != http.StatusOK || response.Body.String() != "outer" {
		t.Fatalf("custom-prefix response = %d %q", response.Code, response.Body.String())
	}
}

func mustProxyHandler(t *testing.T, backendURL, prefix string) http.Handler {
	t.Helper()
	parsed, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(Service{Name: "api", Kind: Proxy, Target: port}, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func prefixWithSlash(prefix string) string {
	if prefix == "/" {
		return prefix
	}
	return prefix + "/"
}

func joinURLPrefix(prefix, name string) string {
	if prefix == "/" {
		return "/" + name
	}
	return fmt.Sprintf("%s/%s", prefix, name)
}
