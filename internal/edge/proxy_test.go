package edge

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRegistryRoutesLongestPrefixAndPreservesEscapedRequest(t *testing.T) {
	requests := make(chan string, 2)
	headers := make(chan http.Header, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests <- request.RequestURI
		headers <- request.Header.Clone()
		_, _ = response.Write([]byte("origin"))
	}))
	defer backend.Close()
	endpoint := testHTTPServerEndpoint(t, backend)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	defer registry.Close()
	if err := registry.Replace([]PublishedRoute{
		{Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "app"}, Origin: testResolvedOrigin(originID, endpoint, now)},
		{Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "app/admin"}, Origin: testResolvedOrigin(originID, endpoint, now)},
	}); err != nil {
		t.Fatal(err)
	}

	request := publicRequest(http.MethodGet, "app.shaulavo.dev", "/app/admin/a%2Fb?q=secret%2Fvalue")
	request.RemoteAddr = "[2001:db8:1:2::1234]:4321"
	request.Header.Set("Forwarded", "for=attacker")
	request.Header.Set("X-Forwarded-For", "203.0.113.8")
	request.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "origin" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if got := <-requests; got != "/app/admin/a%2Fb?q=secret%2Fvalue" {
		t.Fatalf("origin RequestURI = %q", got)
	}
	gotHeaders := <-headers
	if gotHeaders.Get("Forwarded") != "" || gotHeaders.Get("X-Forwarded-For") != "2001:db8:1:2::1234" {
		t.Fatalf("untrusted forwarding headers survived: %#v", gotHeaders)
	}
	if gotHeaders.Get("X-Forwarded-Proto") != "https" || gotHeaders.Get("X-Forwarded-Host") != "app.shaulavo.dev" {
		t.Fatalf("rebuilt forwarding headers = %#v", gotHeaders)
	}
}

func TestProxyModeTrustsOnlyLoopbackForwardedScheme(t *testing.T) {
	gotScheme := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		gotScheme <- request.Header.Get("X-Forwarded-Proto")
		response.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeProxy, now)
	defer registry.Close()
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app"},
		Origin: testResolvedOrigin(originID, testHTTPServerEndpoint(t, backend), now),
	}}); err != nil {
		t.Fatal(err)
	}
	request := publicRequest(http.MethodGet, "app.shaulavo.dev", "/app")
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || <-gotScheme != "https" {
		t.Fatalf("proxy response = %d, scheme did not survive", recorder.Code)
	}

	request = publicRequest(http.MethodGet, "app.shaulavo.dev", "/app")
	request.RemoteAddr = "203.0.113.9:12345"
	recorder = httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("non-loopback proxy request = %d, want 404", recorder.Code)
	}
}

func TestRegistryHardReservesTerminalAndRejectsMalformedPublicRequests(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app"},
		Origin: testResolvedOrigin(originID, netip.MustParseAddrPort("127.0.0.1:9"), now),
	}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ host, path string }{
		{"app.shaulavo.dev", "/mesh"}, {"app.shaulavo.dev", "/mesh/control"},
		{"app.shaulavo.dev", "/m%65sh"}, {"app.shaulavo.dev", "/m%2565sh/control"},
		{"app.shaulavo.dev", "/m%252565sh/control"}, {"app.shaulavo.dev", "/m%25252565sh/control"},
		{"mesh.shaulavo.dev", "/app"}, {"shaulavo.dev", "/app"}, {"a.b.shaulavo.dev", "/app"},
	} {
		request := publicRequest(http.MethodGet, target.host, target.path)
		recorder := httptest.NewRecorder()
		registry.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s%s = %d, want 404", target.host, target.path, recorder.Code)
		}
	}
	request := publicRequest(http.MethodConnect, "app.shaulavo.dev", "/app")
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("CONNECT = %d, want 404", recorder.Code)
	}
	request = publicRequest(http.MethodGet, "app.shaulavo.dev", "/app")
	request.URL.Scheme = "http"
	request.URL.Host = "app.shaulavo.dev"
	recorder = httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("absolute form = %d, want 404", recorder.Code)
	}
	request = publicRequest(http.MethodGet, "app.shaulavo.dev", "/app")
	request.TLS.ServerName = "other.shaulavo.dev"
	recorder = httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("mismatched SNI = %d, want 404", recorder.Code)
	}
	request = publicRequest(http.MethodGet, "app.shaulavo.dev", "/app")
	request.TLS = nil
	recorder = httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("plaintext direct request = %d, want 404", recorder.Code)
	}
}

func TestRegistryHardReservesConfiguredTerminalPathWithoutBlockingOtherEscapes(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry, err := NewRegistry(HandlerConfig{
		Mode: ModeDirectTLS, ReservedPath: "/control/ws", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "control"},
		Origin: testResolvedOrigin(originID, netip.MustParseAddrPort("127.0.0.1:9"), now),
	}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/control/ws", "/control/ws/terminal", "/control%2fws", "/control%252fws/terminal"} {
		recorder := httptest.NewRecorder()
		registry.ServeHTTP(recorder, publicRequest(http.MethodGet, "app.shaulavo.dev", target))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("configured terminal path %q = %d, want 404", target, recorder.Code)
		}
	}
	parsed, err := url.Parse("/app/%2520")
	if err != nil {
		t.Fatal(err)
	}
	if reservedTerminalPath(parsed, "/control/ws") {
		t.Fatal("double-encoded non-terminal path was reserved")
	}
}

func TestOfflineResponseIsSafeAndEscapesDisplayAlias(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "internal/path"},
		Origin: ResolvedOrigin{Identity: originID, DisplayAlias: "Desk <one>", LastSeenAt: now.Add(-time.Minute)},
	}}); err != nil {
		t.Fatal(err)
	}
	request := publicRequest(http.MethodGet, "app.shaulavo.dev", "/internal/path")
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusBadGateway || !strings.Contains(body, "Desk &lt;one&gt;") || !strings.Contains(body, "2026-08-30T11:59:00Z") {
		t.Fatalf("offline response = %d %q", recorder.Code, body)
	}
	for _, secret := range []string{originID, "100.64", "internal/path"} {
		if strings.Contains(body, secret) {
			t.Fatalf("offline response leaked %q: %q", secret, body)
		}
	}
}

func TestProxyDialFailureReturnsPromptSafeBadGateway(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := netip.MustParseAddrPort(listener.Addr().String())
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	if err := registry.Replace([]PublishedRoute{{
		Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "app"}, Origin: testResolvedOrigin(originID, endpoint, now),
	}}); err != nil {
		t.Fatal(err)
	}
	request := publicRequest(http.MethodGet, "app.shaulavo.dev", "/app")
	recorder := httptest.NewRecorder()
	started := time.Now()
	registry.ServeHTTP(recorder, request)
	if elapsed := time.Since(started); recorder.Code != http.StatusBadGateway || elapsed > time.Second {
		t.Fatalf("dial failure = %d after %s", recorder.Code, elapsed)
	}
	if strings.Contains(recorder.Body.String(), endpoint.String()) || strings.Contains(recorder.Body.String(), originID) {
		t.Fatalf("dial failure leaked internals: %q", recorder.Body.String())
	}
}

func TestClientRateStateStaysBounded(t *testing.T) {
	limiter := newClientRateLimiter(4, 2, time.Minute)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	for index := 1; index <= 20; index++ {
		if !limiter.Allow(netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", index)), now.Add(time.Duration(index)*time.Second)) {
			t.Fatal("new bounded client rejected")
		}
	}
	if len(limiter.entries) != 4 {
		t.Fatalf("rate entries = %d, want 4", len(limiter.entries))
	}
	limiter = newClientRateLimiter(4, 2, time.Minute)
	address := netip.MustParseAddr("198.51.100.1")
	if !limiter.Allow(address, now) {
		t.Fatal("first per-client request was rejected")
	}
	if !limiter.Allow(address, now) {
		t.Fatal("second per-client request was rejected")
	}
	if limiter.Allow(address, now) {
		t.Fatal("third per-client request was admitted")
	}
	for round := 1; round <= 10; round++ {
		if !limiter.Allow(address, now.Add(time.Duration(round)*time.Minute)) {
			t.Fatal("expired existing client did not reset")
		}
		for index := 1; index <= 8; index++ {
			limiter.Allow(netip.MustParseAddr(fmt.Sprintf("203.0.%d.%d", round, index)), now.Add(time.Duration(round)*time.Minute))
			if len(limiter.entries) > 4 {
				t.Fatalf("rate entries grew to %d", len(limiter.entries))
			}
		}
	}
}

func TestClientRateLimiterPreservesLiveEntriesAtCapacity(t *testing.T) {
	limiter := newClientRateLimiter(2, 1, time.Minute)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	first := netip.MustParseAddr("192.0.2.1")
	second := netip.MustParseAddr("192.0.2.2")
	if !limiter.Allow(first, now) || !limiter.Allow(second, now) || limiter.Allow(first, now) {
		t.Fatal("initial stable-address limits were not enforced")
	}
	for index := 1; index <= 20; index++ {
		if !limiter.Allow(netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", index)), now) {
			t.Fatal("an untracked rotating address was rejected")
		}
	}
	if limiter.Allow(first, now) {
		t.Fatal("address rotation evicted a live limited client")
	}
	if len(limiter.entries) != 2 {
		t.Fatalf("rate entries = %d, want 2", len(limiter.entries))
	}
	if !limiter.Allow(netip.MustParseAddr("203.0.113.1"), now.Add(time.Minute)) {
		t.Fatal("an expired slot was not reused")
	}
}

func TestClientRateLimiterReclaimsExpiredSlotsBehindLiveEntries(t *testing.T) {
	limiter := newClientRateLimiter(2, 10, time.Minute)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	first := netip.MustParseAddr("192.0.2.1")
	second := netip.MustParseAddr("192.0.2.2")
	if !limiter.Allow(first, now) || !limiter.Allow(second, now) || !limiter.Allow(first, now.Add(time.Minute)) {
		t.Fatal("failed to prepare live and expired entries")
	}
	if !limiter.Allow(netip.MustParseAddr("198.51.100.1"), now.Add(time.Minute)) {
		t.Fatal("untracked address was rejected behind a live entry")
	}
	replacement := netip.MustParseAddr("198.51.100.2")
	if !limiter.Allow(replacement, now.Add(time.Minute)) {
		t.Fatal("expired slot was not reusable")
	}
	if _, exists := limiter.entries[clientQuotaKey(second)]; exists {
		t.Fatal("expired entry remained after its slot was probed")
	}
	if _, exists := limiter.entries[clientQuotaKey(replacement)]; !exists {
		t.Fatal("replacement address was not tracked")
	}
}

func TestIPv6ClientQuotasShareOnePrefix(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	rate := newClientRateLimiter(4, 2, time.Minute)
	for index, want := range []bool{true, true, false} {
		address := netip.MustParseAddr(fmt.Sprintf("2001:db8:1:2::%d", index+1))
		if got := rate.Allow(address, now); got != want {
			t.Fatalf("rate request %d = %t, want %t", index+1, got, want)
		}
	}
	if !rate.Allow(netip.MustParseAddr("2001:db8:1:3::1"), now) {
		t.Fatal("a different IPv6 /64 shared the rate quota")
	}

	concurrency := newClientConcurrencyLimiter(2)
	first := netip.MustParseAddr("2001:db8:4:5::1")
	second := netip.MustParseAddr("2001:db8:4:5::2")
	third := netip.MustParseAddr("2001:db8:4:5::3")
	if !concurrency.Acquire(first) || !concurrency.Acquire(second) || concurrency.Acquire(third) {
		t.Fatal("rotating addresses bypassed the IPv6 /64 concurrency quota")
	}
	concurrency.Release(first)
	concurrency.Release(second)
	if !concurrency.Acquire(third) {
		t.Fatal("released IPv6 /64 quota was not reusable")
	}
	concurrency.Release(third)
	if !concurrency.Acquire(netip.MustParseAddr("2001:db8:4:6::1")) {
		t.Fatal("a different IPv6 /64 shared the concurrency quota")
	}
}

func TestRegistryRequestBodyBoundsDoNotChangeOriginLiveness(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = response.Write([]byte("healthy"))
	}))
	defer backend.Close()
	endpoint := testHTTPServerEndpoint(t, backend)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	var dials atomic.Int64
	dialer := &net.Dialer{Timeout: time.Second}
	registry, err := NewRegistry(HandlerConfig{
		Mode: ModeDirectTLS, Now: func() time.Time { return now }, RequestBodyLimit: 8,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return dialer.DialContext(ctx, network, address)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Replace([]PublishedRoute{{
		Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "app"}, Origin: testResolvedOrigin(originID, endpoint, now),
	}}); err != nil {
		t.Fatal(err)
	}

	known := publicRequestWithBody(http.MethodPost, "app.shaulavo.dev", "/app", strings.NewReader("123456789"))
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, known)
	if recorder.Code != http.StatusRequestEntityTooLarge || dials.Load() != 0 {
		t.Fatalf("known overflow = %d with %d dials", recorder.Code, dials.Load())
	}

	chunked := publicRequestWithBody(http.MethodPost, "app.shaulavo.dev", "/app", strings.NewReader("123456789"))
	chunked.ContentLength = -1
	recorder = httptest.NewRecorder()
	registry.ServeHTTP(recorder, chunked)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked overflow = %d %q", recorder.Code, recorder.Body.String())
	}

	normal := publicRequest(http.MethodGet, "app.shaulavo.dev", "/app")
	recorder = httptest.NewRecorder()
	registry.ServeHTTP(recorder, normal)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "healthy" {
		t.Fatalf("healthy request after overflow = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestRegistryReturnsRequestTimeoutForInboundBodyDeadline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		response.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	defer registry.Close()
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app"},
		Origin: testResolvedOrigin(originID, testHTTPServerEndpoint(t, backend), now),
	}}); err != nil {
		t.Fatal(err)
	}
	request := publicRequest(http.MethodPost, "app.shaulavo.dev", "/app")
	request.Body = timeoutReadCloser{}
	request.ContentLength = -1
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusRequestTimeout {
		t.Fatalf("body timeout response = %d %q, want 408", response.Code, response.Body.String())
	}
}

type timeoutReadCloser struct{}

func (timeoutReadCloser) Read([]byte) (int, error) { return 0, timeoutError{} }
func (timeoutReadCloser) Close() error             { return nil }

type timeoutError struct{}

func (timeoutError) Error() string { return "read deadline exceeded" }
func (timeoutError) Timeout() bool { return true }

func TestRegistryProxiesWebSocketUpgrade(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(response, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow() //nolint:errcheck // test peer cleanup
		messageType, payload, err := connection.Read(request.Context())
		if err == nil {
			_ = connection.Write(request.Context(), messageType, payload)
		}
	}))
	defer backend.Close()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	defer registry.Close()
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app"},
		Origin: testResolvedOrigin(originID, testHTTPServerEndpoint(t, backend), now),
	}}); err != nil {
		t.Fatal(err)
	}
	edgeServer := httptest.NewTLSServer(registry)
	defer edgeServer.Close()
	_, edgePort, err := net.SplitHostPort(strings.TrimPrefix(edgeServer.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	dialer := &net.Dialer{Timeout: time.Second}
	httpClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, //nolint:gosec // local test certificate
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, edgeServer.Listener.Addr().String())
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, "wss://app.shaulavo.dev:"+edgePort+"/app/socket", &websocket.DialOptions{HTTPClient: httpClient}) //nolint:bodyclose // websocket.Dial owns and closes its HTTP response body
	if response != nil && response.Body != nil {
		defer response.Body.Close() //nolint:errcheck // test cleanup
	}
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow() //nolint:errcheck // test cleanup
	payload := []byte{0, 1, 2, 255}
	if err := connection.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	messageType, got, err := connection.Read(ctx)
	if err != nil || messageType != websocket.MessageBinary || string(got) != string(payload) {
		t.Fatalf("WebSocket echo = type %v payload %v error %v", messageType, got, err)
	}
}

func TestRegistryBoundsConcurrentRequestsPerClient(t *testing.T) {
	entered := make(chan struct{}, maximumConcurrentPerClient+1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		response.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	defer registry.Close()
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app"},
		Origin: testResolvedOrigin(originID, testHTTPServerEndpoint(t, backend), now),
	}}); err != nil {
		t.Fatal(err)
	}
	results := make(chan int, maximumConcurrentPerClient+1)
	for range maximumConcurrentPerClient + 1 {
		go func() {
			recorder := httptest.NewRecorder()
			registry.ServeHTTP(recorder, publicRequest(http.MethodGet, "app.shaulavo.dev", "/app"))
			results <- recorder.Code
		}()
	}
	for range maximumConcurrentPerClient {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("bounded upstream request did not start")
		}
	}
	select {
	case status := <-results:
		if status != http.StatusServiceUnavailable {
			t.Fatalf("ninth concurrent response = %d, want 503", status)
		}
	case <-time.After(time.Second):
		t.Fatal("ninth concurrent request did not fail promptly")
	}
	close(release)
	for range maximumConcurrentPerClient {
		if status := <-results; status != http.StatusNoContent {
			t.Fatalf("admitted concurrent response = %d, want 204", status)
		}
	}
}

func TestRegistryRetainsPerOriginAndGlobalBudgetsAcrossReplacement(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	published := []PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app"},
		Origin: testResolvedOrigin(originID, netip.MustParseAddrPort("127.0.0.1:9"), now),
	}}
	if err := registry.Replace(published); err != nil {
		t.Fatal(err)
	}
	budget := registry.budgets[originID]
	for range maximumConcurrentPerOrigin {
		budget <- struct{}{}
	}
	if err := registry.Replace(published); err != nil {
		t.Fatal(err)
	}
	if registry.budgets[originID] != budget || len(registry.budgets[originID]) != maximumConcurrentPerOrigin {
		t.Fatal("replacement reset the per-origin concurrency budget")
	}
	if cap(registry.global) != maximumConcurrentUpstreams {
		t.Fatalf("global upstream budget = %d", cap(registry.global))
	}
}

func TestCanonicalPublicHostAndForwardedIP(t *testing.T) {
	host, forwarded, err := canonicalPublicHost("app.shaulavo.dev:443")
	if err != nil || host != "app.shaulavo.dev" || forwarded != "app.shaulavo.dev:443" {
		t.Fatalf("canonical host = %q %q, %v", host, forwarded, err)
	}
	for _, invalid := range []string{
		"APP.shaulavo.dev", "app.shaulavo.dev:0", "app.shaulavo.dev:0443", "app.shaulavo.dev:65536",
		"user@app.shaulavo.dev", ":443", "[app.shaulavo.dev]:443",
	} {
		if _, _, err := canonicalPublicHost(invalid); err == nil {
			t.Fatalf("invalid host %q accepted", invalid)
		}
	}
	for _, invalid := range []string{"", "198.51.100.1, 127.0.0.1", " 198.51.100.1", "fe80::1%eth0"} {
		if _, err := canonicalForwardedIP(invalid); err == nil {
			t.Fatalf("invalid forwarded IP %q accepted", invalid)
		}
	}
	header := http.Header{"X-Forwarded-For": {"198.51.100.1", "198.51.100.2"}, "X-Forwarded-Proto": {"https"}}
	if _, err := trustedForwardedMetadata(header); err == nil {
		t.Fatal("duplicate forwarded client IP accepted")
	}
	header = http.Header{"X-Forwarded-For": {"198.51.100.1"}, "X-Forwarded-Proto": {"https", "http"}}
	if _, err := trustedForwardedMetadata(header); err == nil {
		t.Fatal("duplicate forwarded scheme accepted")
	}
	mapped, err := immediateClientIP("[::ffff:192.0.2.1]:443")
	if err != nil || mapped.String() != "192.0.2.1" {
		t.Fatalf("mapped immediate address = %v, %v", mapped, err)
	}
}

func TestListCursorProgressesAcrossHeartbeatOnlyReplacement(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, now)
	endpoint := netip.MustParseAddrPort("127.0.0.1:9")
	build := func(lastSeen time.Time) []PublishedRoute {
		origin := testResolvedOrigin(originID, endpoint, lastSeen)
		return []PublishedRoute{
			{Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "a"}, Origin: origin},
			{Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "b"}, Origin: origin},
			{Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "c"}, Origin: origin},
		}
	}
	if err := registry.Replace(build(now)); err != nil {
		t.Fatal(err)
	}
	first, cursor, err := registry.Page("", 1)
	if err != nil || len(first) != 1 || first[0].ServiceName != "a" || cursor == "" {
		t.Fatalf("first page = %#v, cursor %q, error %v", first, cursor, err)
	}
	if err := registry.Replace(build(now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	second, _, err := registry.Page(cursor, 1)
	if err != nil || len(second) != 1 || second[0].ServiceName != "b" {
		t.Fatalf("second page after heartbeat = %#v, error %v", second, err)
	}
	if _, _, err := registry.Page(strings.Repeat("A", maximumListCursorLength+1), 1); err == nil {
		t.Fatal("oversized cursor accepted")
	}
}

func TestWakeWaitsForFreshAuthenticatedGeneration(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("awake"))
	}))
	defer backend.Close()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	originID, _ := testIdentity(t)
	var registry *Registry
	waker := wakerFunc(func(context.Context, string) error {
		return registry.Replace([]PublishedRoute{{
			Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app", WakeOnRequest: true},
			Origin: testResolvedOrigin(originID, testHTTPServerEndpoint(t, backend), now),
		}})
	})
	var err error
	registry, err = NewRegistry(HandlerConfig{Mode: ModeDirectTLS, Now: func() time.Time { return now }, Waker: waker, WakeTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app", WakeOnRequest: true},
		Origin: ResolvedOrigin{Identity: originID, DisplayAlias: "Desktop", LastSeenAt: now.Add(-time.Minute)},
	}}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, publicRequest(http.MethodGet, "app.shaulavo.dev", "/app"))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "awake" {
		t.Fatalf("wake response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestWakeTimeoutOrOwnershipTransferCannotDispatch(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	firstID, _ := testIdentity(t)
	secondID, _ := testIdentity(t)
	for name, replace := range map[string]bool{"no heartbeat": false, "ownership transfer": true} {
		t.Run(name, func(t *testing.T) {
			var registry *Registry
			waker := wakerFunc(func(context.Context, string) error {
				if replace {
					return registry.Replace([]PublishedRoute{{
						Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app", WakeOnRequest: true},
						Origin: testResolvedOrigin(secondID, netip.MustParseAddrPort("127.0.0.1:9"), now),
					}})
				}
				return nil
			})
			var err error
			registry, err = NewRegistry(HandlerConfig{Mode: ModeDirectTLS, Now: func() time.Time { return now }, Waker: waker, WakeTimeout: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Replace([]PublishedRoute{{
				Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app", WakeOnRequest: true},
				Origin: ResolvedOrigin{Identity: firstID, DisplayAlias: "Desktop", LastSeenAt: now.Add(-time.Minute)},
			}}); err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			registry.ServeHTTP(recorder, publicRequest(http.MethodGet, "app.shaulavo.dev", "/app"))
			if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "Desktop") {
				t.Fatalf("wake timeout response = %d %q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func testRegistry(t *testing.T, mode Mode, now time.Time) *Registry {
	t.Helper()
	registry, err := NewRegistry(HandlerConfig{Mode: mode, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func testResolvedOrigin(identity string, endpoint netip.AddrPort, now time.Time) ResolvedOrigin {
	return ResolvedOrigin{
		Identity: identity, DisplayAlias: "Desktop", Endpoint: endpoint,
		SnapshotSequence: 1, LastSeenAt: now, OnlineUntil: now.Add(5 * time.Minute), Online: true,
	}
}

func testHTTPServerEndpoint(t *testing.T, server *httptest.Server) netip.AddrPort {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := netip.ParseAddrPort(net.JoinHostPort(host, port))
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func publicRequest(method, host, target string) *http.Request {
	return publicRequestWithBody(method, host, target, nil)
}

func publicRequestWithBody(method, host, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Host = host
	serverName := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		serverName = parsedHost
	}
	request.TLS = &tls.ConnectionState{ServerName: serverName}
	return request
}

type wakerFunc func(context.Context, string) error

func (function wakerFunc) Wake(ctx context.Context, originID string) error {
	return function(ctx, originID)
}
