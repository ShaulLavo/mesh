package edge

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tunnel"
)

const proxyTunnelName = "blog.shaulavo.dev"

type proxyTunnelState struct{ claim tunnel.Claim }

func (s *proxyTunnelState) TunnelVersion(context.Context, string) (tunnel.Ack, error) {
	return tunnel.Ack{}, tunnel.ErrNotFound
}
func (s *proxyTunnelState) TunnelClaim(_ context.Context, name string) (tunnel.Claim, error) {
	if name != s.claim.PublicName {
		return tunnel.Claim{}, tunnel.ErrNotFound
	}
	return s.claim, nil
}
func (*proxyTunnelState) ApplyTunnelMutation(context.Context, tunnel.Mutation, string) error {
	return errors.New("test only contains a restored claim")
}
func (*proxyTunnelState) DeleteTunnelClaim(context.Context, string) error {
	return errors.New("test does not delete durable claims")
}

type proxyTunnelEndpoint struct {
	dial   func(context.Context) (net.Conn, error)
	closed atomic.Int32
}

func (e *proxyTunnelEndpoint) Dial(ctx context.Context) (net.Conn, error) { return e.dial(ctx) }
func (e *proxyTunnelEndpoint) Close() error                               { e.closed.Add(1); return nil }

func newProxyTunnel(t *testing.T) (*Controller, *Registry, string) {
	t.Helper()
	edgeID, _ := testIdentity(t)
	claimant, _ := testIdentity(t)
	originID, _ := testIdentity(t)
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	controller, err := NewController(context.Background(), ControllerConfig{
		TargetID: edgeID, Origins: []OriginConfig{testOriginConfig(originID)},
		State: newMemoryStateStore(), Registry: registry,
		Resolve:         func(context.Context, OriginConfig) (netip.AddrPort, error) { return netip.AddrPort{}, nil },
		Pin:             func(context.Context, netip.AddrPort, OriginConfig) error { return nil },
		TunnelState:     &proxyTunnelState{tunnel.Claim{PublicName: proxyTunnelName, ClaimantID: claimant}},
		AuthorizeTunnel: func(id string) bool { return id == claimant },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.CloseTunnels)
	return controller, registry, claimant
}

func proxyEndpointFor(server *httptest.Server) *proxyTunnelEndpoint {
	return &proxyTunnelEndpoint{dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
	}}
}

func activateProxyTunnel(t *testing.T, controller *Controller, claimant string, endpoint tunnel.Endpoint) func() {
	t.Helper()
	release, err := controller.ActivateTunnel(context.Background(), claimant, proxyTunnelName, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

type bodyTimeoutTransport struct{}

func (bodyTimeoutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	_, _ = io.ReadAll(request.Body)
	return nil, io.ErrUnexpectedEOF
}

func TestTunnelBodyTimeoutSurvivesUpstreamError(t *testing.T) {
	controller, registry, claimant := newProxyTunnel(t)
	endpoint := &proxyTunnelEndpoint{dial: func(context.Context) (net.Conn, error) {
		return nil, errors.New("test transport does not dial")
	}}
	activateProxyTunnel(t, controller, claimant, endpoint)
	route := registry.findTunnel(proxyTunnelName)
	route.proxy.Transport = bodyTimeoutTransport{}
	request := publicRequest(http.MethodPost, proxyTunnelName, "/upload")
	request.Body = timeoutReadCloser{}
	request.ContentLength = -1
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusRequestTimeout {
		t.Fatalf("response = %d, want 408", response.Code)
	}
	if !route.active.Load() {
		t.Fatal("request timeout deactivated the tunnel")
	}
}

func TestTunnelHTTPKeepsPathBodyAndTrustedHeaders(t *testing.T) {
	controller, registry, claimant := newProxyTunnel(t)
	before := httptest.NewRecorder()
	registry.ServeHTTP(before, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if before.Code != http.StatusNotFound {
		t.Fatalf("inactive claim = %d", before.Code)
	}
	received := make(chan *http.Request, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Header.Set("Test-Body", string(body))
		received <- r
		_, _ = io.WriteString(w, "laptop response")
	}))
	defer origin.Close()
	release := activateProxyTunnel(t, controller, claimant, proxyEndpointFor(origin))
	request := publicRequestWithBody(http.MethodPost, proxyTunnelName, "/a%2Fb?q=x%2Fy", strings.NewReader("request body"))
	request.Header.Set("Forwarded", "for=untrusted")
	request.Header.Set("X-Forwarded-For", "203.0.113.200")
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "laptop response" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	got := <-received
	if got.Method != http.MethodPost || got.RequestURI != "/a%2Fb?q=x%2Fy" || got.Header.Get("Test-Body") != "request body" {
		t.Fatalf("upstream request = %s %s %#v", got.Method, got.RequestURI, got.Header)
	}
	if got.Host != proxyTunnelName || got.Header.Get("Forwarded") != "" || got.Header.Get("X-Forwarded-For") == "203.0.113.200" {
		t.Fatalf("untrusted headers survived: %#v", got.Header)
	}
	release()
	after := httptest.NewRecorder()
	registry.ServeHTTP(after, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if after.Code != http.StatusNotFound {
		t.Fatalf("disconnected route = %d", after.Code)
	}
}

func TestTunnelHTTPStreamsBeforeOriginFinishes(t *testing.T) {
	controller, registry, claimant := newProxyTunnel(t)
	finish := make(chan struct{})
	defer close(finish)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-finish
		_, _ = io.WriteString(w, "last\n")
	}))
	t.Cleanup(origin.Close)
	activateProxyTunnel(t, controller, claimant, proxyEndpointFor(origin))
	edgeServer := httptest.NewTLSServer(registry)
	defer edgeServer.Close()
	client := edgeServer.Client()
	client.Timeout = time.Second
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{ServerName: proxyTunnelName, InsecureSkipVerify: true} //nolint:gosec // local TLS test server
	request, err := http.NewRequest(http.MethodGet, edgeServer.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = proxyTunnelName
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("first streamed line = %q, %v", line, err)
	}
}

func TestTunnelTerminalPathsNeverDial(t *testing.T) {
	controller, registry, claimant := newProxyTunnel(t)
	var dials atomic.Int32
	endpoint := &proxyTunnelEndpoint{dial: func(context.Context) (net.Conn, error) { dials.Add(1); return nil, errors.New("must not dial") }}
	activateProxyTunnel(t, controller, claimant, endpoint)
	for _, path := range []string{"/mesh", "/mesh/session", "/m%65sh", "/a/../mesh", "/mesh%2fsession"} {
		recorder := httptest.NewRecorder()
		registry.ServeHTTP(recorder, publicRequest(http.MethodGet, proxyTunnelName, path))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("terminal path %q = %d", path, recorder.Code)
		}
		if dials.Load() != 0 {
			t.Fatalf("terminal path %q reached the SSH endpoint", path)
		}
	}
	if dials.Load() != 0 {
		t.Fatal("terminal request reached the SSH endpoint")
	}
}

func TestTunnelDialFailureWithdrawsOnlyItsActivation(t *testing.T) {
	controller, registry, claimant := newProxyTunnel(t)
	broken := &proxyTunnelEndpoint{dial: func(context.Context) (net.Conn, error) { return nil, errors.New("private-machine.mesh: disconnected") }}
	oldRelease := activateProxyTunnel(t, controller, claimant, broken)
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if recorder.Code != http.StatusNotFound || strings.Contains(recorder.Body.String(), "private-machine") {
		t.Fatalf("dial failure leaked/stayed active: %d %q", recorder.Code, recorder.Body.String())
	}
	if registry.findTunnel(proxyTunnelName) != nil || broken.closed.Load() != 1 {
		t.Fatal("failed endpoint was not removed and closed")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "new connection") }))
	defer origin.Close()
	replacement := proxyEndpointFor(origin)
	activateProxyTunnel(t, controller, claimant, replacement)
	var stale sync.WaitGroup
	for range 16 {
		stale.Go(oldRelease)
	}
	stale.Wait()
	recorder = httptest.NewRecorder()
	registry.ServeHTTP(recorder, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "new connection" || replacement.closed.Load() != 0 {
		t.Fatalf("old token removed replacement: %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestTunnelCapacityKeepsClaimActive(t *testing.T) {
	controller, registry, claimant := newProxyTunnel(t)
	endpoint := &proxyTunnelEndpoint{dial: func(context.Context) (net.Conn, error) { return nil, tunnel.ErrCapacity }}
	activateProxyTunnel(t, controller, claimant, endpoint)
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if recorder.Code != http.StatusServiceUnavailable || registry.findTunnel(proxyTunnelName) == nil || endpoint.closed.Load() != 0 {
		t.Fatal("capacity rejection deactivated the tunnel")
	}
}

func TestTunnelPublicConcurrencyBounds(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global=%t", global), func(t *testing.T) { exerciseTunnelConcurrency(t, global) })
	}
}

func exerciseTunnelConcurrency(t *testing.T, global bool) {
	t.Helper()
	controller, registry, claimant := newProxyTunnel(t)
	limit := maximumConcurrentPerClient
	if global {
		limit = maximumConcurrentUpstreams
	}
	entered := make(chan struct{}, limit+1)
	finish := make(chan struct{})
	defer close(finish)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-finish
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(origin.Close)
	activateProxyTunnel(t, controller, claimant, proxyEndpointFor(origin))
	var pending sync.WaitGroup
	for index := range limit {
		request := publicRequest(http.MethodGet, proxyTunnelName, "/")
		if global {
			request.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", index+1)
		}
		pending.Go(func() { registry.ServeHTTP(httptest.NewRecorder(), request) })
	}
	for range limit {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("public requests did not occupy the concurrency budget")
		}
	}
	request := publicRequest(http.MethodGet, proxyTunnelName, "/")
	if global {
		request.RemoteAddr = "198.51.100.250:1234"
	}
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("overflow request = %d", recorder.Code)
	}
	t.Cleanup(pending.Wait)
}

func TestTunnelPublicRateBoundPrecedesDial(t *testing.T) {
	controller, registry, claimant := newProxyTunnel(t)
	var dials atomic.Int32
	endpoint := &proxyTunnelEndpoint{dial: func(context.Context) (net.Conn, error) { dials.Add(1); return nil, tunnel.ErrCapacity }}
	activateProxyTunnel(t, controller, claimant, endpoint)
	for range maximumRequestsPerMinute {
		registry.ServeHTTP(httptest.NewRecorder(), publicRequest(http.MethodGet, proxyTunnelName, "/"))
	}
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if recorder.Code != http.StatusTooManyRequests || dials.Load() != maximumRequestsPerMinute {
		t.Fatalf("rate overflow = %d, dials = %d", recorder.Code, dials.Load())
	}
}
