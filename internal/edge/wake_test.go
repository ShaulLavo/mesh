package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
)

func TestWakeResolverRetainsOfflineAddressesWithoutWeakeningRegistration(t *testing.T) {
	origin := OriginConfig{TailscaleName: "pc.example.ts.net", ControlPort: 7337}
	peers := func(context.Context) ([]tailnet.Peer, error) {
		return []tailnet.Peer{{Name: origin.TailscaleName, Addrs: []string{"fd7a:115c:a1e0::8", "100.64.0.8"}}}, nil
	}
	endpoint, err := TailscaleWakeResolver(peers)(context.Background(), origin)
	if err != nil || endpoint.String() != "100.64.0.8:7337" {
		t.Fatalf("offline wake endpoint = %s, %v", endpoint, err)
	}
	if _, err := TailscaleResolver(peers)(context.Background(), origin); err == nil {
		t.Fatal("registration accepted an offline peer")
	}
	for _, invalid := range [][]tailnet.Peer{
		{{Name: "other.example.ts.net", Addrs: []string{"100.64.0.8"}}},
		{{Name: origin.TailscaleName, Addrs: []string{"203.0.113.8"}}},
		{{Name: origin.TailscaleName, Addrs: []string{"100.64.0.8"}}, {Name: origin.TailscaleName, Addrs: []string{"100.64.0.9"}}},
	} {
		resolver := TailscaleWakeResolver(func(context.Context) ([]tailnet.Peer, error) { return invalid, nil })
		if _, err := resolver(context.Background(), origin); err == nil {
			t.Fatalf("wake resolver accepted invalid peers: %#v", invalid)
		}
	}
}

func TestFreshHeartbeatDoesNotPreventWakeBeforeHTTPBodyIsSent(t *testing.T) {
	var received atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		received.Add(1)
		contents, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		_, _ = response.Write(contents)
	}))
	defer backend.Close()
	now := time.Now()
	originID, _ := testIdentity(t)
	endpoint := testHTTPServerEndpoint(t, backend)
	publication := []PublishedRoute{{
		Route:  Route{PublicName: "app.shaulavo.dev", ServiceName: "app", WakeOnRequest: true},
		Origin: testResolvedOrigin(originID, endpoint, now),
	}}
	body := &wakeTrackedReader{reader: strings.NewReader("one mutation")}
	var awake atomic.Bool
	wakes := 0
	var registry *Registry
	waker := wakerFunc(func(context.Context, string) error {
		wakes++
		if body.reads.Load() != 0 || received.Load() != 0 {
			t.Error("HTTP body was consumed before waking")
		}
		awake.Store(true)
		publication[0].Origin.SnapshotSequence++
		return registry.Replace(publication)
	})
	var err error
	registry, err = NewRegistry(HandlerConfig{
		Mode: ModeDirectTLS, Waker: waker, WakeTimeout: time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if !awake.Load() {
				return nil, errors.New("sleeping origin")
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Replace(publication); err != nil {
		t.Fatal(err)
	}
	request := publicRequestWithBody(http.MethodPost, "app.shaulavo.dev", "/app", body)
	request.ContentLength = int64(len("one mutation"))
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "one mutation" || wakes != 1 || received.Load() != 1 {
		t.Fatalf("response=%d %q, wakes=%d, HTTP requests=%d", response.Code, response.Body.String(), wakes, received.Load())
	}
}

type wakeTrackedReader struct {
	reader io.Reader
	reads  atomic.Int32
}

func (r *wakeTrackedReader) Read(contents []byte) (int, error) {
	r.reads.Add(1)
	return r.reader.Read(contents)
}

func TestSleepingOriginsDoNotConsumeHealthyUpstreamCapacity(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("healthy"))
	}))
	defer backend.Close()
	asleepID, _ := testIdentity(t)
	healthyID, _ := testIdentity(t)
	entered := make(chan struct{}, maximumConcurrentWakeWaits)
	release := make(chan struct{})
	registry, err := NewRegistry(HandlerConfig{
		Mode: ModeDirectTLS, WakeTimeout: 5 * time.Second,
		Waker: wakerFunc(func(ctx context.Context, _ string) error {
			entered <- struct{}{}
			select {
			case <-release:
				return errors.New("wake unavailable")
			case <-ctx.Done():
				return ctx.Err()
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	now := time.Now()
	if err := registry.Replace([]PublishedRoute{
		{Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "asleep", WakeOnRequest: true}, Origin: ResolvedOrigin{Identity: asleepID, DisplayAlias: "Sleeping", LastSeenAt: now}},
		{Route: Route{PublicName: "app.shaulavo.dev", ServiceName: "healthy", WakeOnRequest: true}, Origin: testResolvedOrigin(healthyID, testHTTPServerEndpoint(t, backend), now)},
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, maximumConcurrentWakeWaits)
	defer func() {
		close(release)
		for range maximumConcurrentWakeWaits {
			<-done
		}
	}()
	for index := range maximumConcurrentWakeWaits {
		go serveSleepingTestRequest(registry, index, done)
	}
	deadline := time.After(2 * time.Second)
	for range maximumConcurrentWakeWaits {
		select {
		case <-entered:
		case <-deadline:
			t.Fatal("wake wait pool did not fill")
		}
	}
	if active := len(registry.global); active != 0 {
		t.Fatalf("sleeping origins hold %d upstream permits", active)
	}
	healthy := httptest.NewRecorder()
	registry.ServeHTTP(healthy, publicRequest(http.MethodGet, "app.shaulavo.dev", "/healthy"))
	if healthy.Code != http.StatusOK || healthy.Body.String() != "healthy" {
		t.Fatalf("healthy origin while wakes wait = %d %q", healthy.Code, healthy.Body.String())
	}
	excess := httptest.NewRecorder()
	registry.ServeHTTP(excess, publicRequest(http.MethodGet, "app.shaulavo.dev", "/asleep"))
	if excess.Code != http.StatusServiceUnavailable {
		t.Fatalf("excess wake waiter status = %d", excess.Code)
	}
}

func serveSleepingTestRequest(registry *Registry, index int, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	request := publicRequest(http.MethodGet, "app.shaulavo.dev", "/asleep")
	request.RemoteAddr = fmt.Sprintf("198.51.100.%d:5000", index+1)
	registry.ServeHTTP(httptest.NewRecorder(), request)
}

func TestWakeRequiresTargetPublication(t *testing.T) {
	t.Run("another origin cannot release the request", func(t *testing.T) {
		testWakePublication(t, false)
	})
	t.Run("a new target publication releases the request", func(t *testing.T) {
		testWakePublication(t, true)
	})
}

func testWakePublication(t *testing.T, publishTarget bool) {
	t.Helper()
	var received atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		response.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	endpoint := testHTTPServerEndpoint(t, backend)
	now := time.Now()
	edgeID, _ := testIdentity(t)
	targetID, targetKey := testIdentity(t)
	otherID, otherKey := testIdentity(t)
	targetConfig := testOriginConfig(targetID)
	otherConfig := testOriginConfig(otherID)
	otherConfig.TailscaleName = "other.example.ts.net"
	state := newMemoryStateStore()
	targetRoutes := []Route{{PublicName: "app.shaulavo.dev", ServiceName: "app", WakeOnRequest: true}}
	var awake atomic.Bool
	var controller *Controller
	registry, err := NewRegistry(HandlerConfig{
		Mode: ModeDirectTLS, Now: func() time.Time { return now }, WakeTimeout: 50 * time.Millisecond,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if !awake.Load() {
				return nil, errors.New("target is asleep")
			}
			return (&net.Dialer{}).DialContext(ctx, network, endpoint.String())
		},
		Waker: wakerFunc(func(ctx context.Context, _ string) error {
			awake.Store(true)
			other := signedRegistrationSnapshot(t, edgeID, otherID, otherKey, 1, now, nil)
			if err := registerWakeTestSnapshot(ctx, controller, other); err != nil {
				return err
			}
			if !publishTarget {
				return nil
			}
			target := signedRegistrationSnapshot(t, edgeID, targetID, targetKey, 2, now, targetRoutes)
			return registerWakeTestSnapshot(ctx, controller, target)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	controller = testController(t, now, edgeID, []OriginConfig{targetConfig, otherConfig}, state, registry,
		func(context.Context, OriginConfig) (netip.AddrPort, error) {
			return netip.MustParseAddrPort("100.64.0.8:7337"), nil
		},
		func(context.Context, netip.AddrPort, OriginConfig) error { return nil },
	)
	target := signedRegistrationSnapshot(t, edgeID, targetID, targetKey, 1, now, targetRoutes)
	if err := registerWakeTestSnapshot(context.Background(), controller, target); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, publicRequest(http.MethodPost, "app.shaulavo.dev", "/app"))
	if !publishTarget {
		if received.Load() != 0 || response.Code != http.StatusBadGateway {
			t.Fatalf("another origin's publication dispatched %d HTTP requests; status = %d", received.Load(), response.Code)
		}
		return
	}
	if received.Load() != 1 || response.Code != http.StatusOK {
		t.Fatalf("target publication dispatched %d HTTP requests; status = %d", received.Load(), response.Code)
	}
}

func registerWakeTestSnapshot(ctx context.Context, controller *Controller, snapshot Snapshot) error {
	_, _, err := controller.HandleControl(ctx, protocol.Control{
		Type: protocol.TypeEdgeRegister, RequestID: "wake-publication", EdgeSnapshot: pointerProtocolSnapshot(snapshot),
	})
	return err
}
