package edge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/protocol"
)

type appHandlerFunc func(http.ResponseWriter, *http.Request, string) bool

func (f appHandlerFunc) ServeHost(w http.ResponseWriter, r *http.Request, name string) bool {
	return f(w, r, name)
}

func TestAppRoutingSurvivesSnapshotReplacementAndConsumesTombstones(t *testing.T) {
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	calls := 0
	registry.SetAppHandler(appHandlerFunc(func(w http.ResponseWriter, r *http.Request, name string) bool {
		calls++
		if name != "7k3d.shaulavo.dev" {
			return false
		}
		http.Error(w, "expired", http.StatusGone)
		return true
	}))
	if err := registry.Replace(nil); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, publicRequest(http.MethodGet, "7k3d.shaulavo.dev", "/api/data"))
	if calls != 1 || response.Code != http.StatusGone {
		t.Fatalf("app routing lost: calls=%d response=%d", calls, response.Code)
	}
	response = httptest.NewRecorder()
	registry.ServeHTTP(response, publicRequest(http.MethodGet, "other.shaulavo.dev", "/api/data"))
	if calls != 2 || response.Code != http.StatusNotFound {
		t.Fatalf("app fallthrough: calls=%d response=%d", calls, response.Code)
	}
}

func TestAppRoutingCannotBypassPublicEntranceChecks(t *testing.T) {
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	calls := 0
	registry.SetAppHandler(appHandlerFunc(func(w http.ResponseWriter, r *http.Request, name string) bool { calls++; return true }))
	plain := publicRequest(http.MethodGet, "7k3d.shaulavo.dev", "/")
	plain.TLS = nil
	registry.ServeHTTP(httptest.NewRecorder(), plain)
	wrongSNI := publicRequest(http.MethodGet, "7k3d.shaulavo.dev", "/")
	wrongSNI.TLS.ServerName = "other.shaulavo.dev"
	registry.ServeHTTP(httptest.NewRecorder(), wrongSNI)
	registry.ServeHTTP(httptest.NewRecorder(), publicRequest(http.MethodGet, "7k3d.other.dev", "/"))
	if calls != 0 {
		t.Fatalf("app handler reached before public checks: calls=%d", calls)
	}
}

func TestAppWholeHostCanServeTerminalNamedPaths(t *testing.T) {
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	registry.SetAppHandler(appHandlerFunc(func(w http.ResponseWriter, r *http.Request, name string) bool {
		if name != "7k3d.shaulavo.dev" {
			return false
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}))
	for _, path := range []string{"/mesh", "/m%65sh/control"} {
		appResponse := httptest.NewRecorder()
		registry.ServeHTTP(appResponse, publicRequest(http.MethodGet, "7k3d.shaulavo.dev", path))
		if appResponse.Code != http.StatusNoContent {
			t.Fatalf("app path %s intercepted: %d", path, appResponse.Code)
		}
		ordinaryResponse := httptest.NewRecorder()
		registry.ServeHTTP(ordinaryResponse, publicRequest(http.MethodGet, "other.shaulavo.dev", path))
		if ordinaryResponse.Code != http.StatusNotFound {
			t.Fatalf("ordinary terminal path %s exposed: %d", path, ordinaryResponse.Code)
		}
	}
}

func TestAppExchangePinsEdgeBeforeSendingSignedOperation(t *testing.T) {
	now := time.Now().UTC()
	state := &publisherMemoryOutbox{}
	exchanges := 0
	publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			return publisherHostResponse(request, "unrelated-edge"), nil
		}
		exchanges++
		return protocol.Control{}, nil
	})
	signed, err := apps.Sign("mesh-app/request/v1", state.targetID, 1, apps.Request{Action: "list"}, publisher.signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.AppExchange(context.Background(), signed); err == nil || exchanges != 0 {
		t.Fatalf("wrong edge received app operation: err=%v exchanges=%d", err, exchanges)
	}
}

func TestAppExchangeRejectsAnotherOwnerBeforeTransport(t *testing.T) {
	now := time.Now().UTC()
	state := &publisherMemoryOutbox{}
	calls := 0
	publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) { calls++; return protocol.Control{}, nil })
	_, otherKey := testIdentity(t)
	signed, err := apps.Sign("mesh-app/request/v1", state.targetID, 1, apps.Request{Action: "list"}, otherKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.AppExchange(context.Background(), signed); err == nil || calls != 0 {
		t.Fatalf("foreign operation reached transport: err=%v calls=%d", err, calls)
	}
}

func TestAppExchangeVerifiesSignedResponseIdentityAndSequence(t *testing.T) {
	now := time.Now().UTC()
	edgeID, edgeKey := testIdentity(t)
	state := &publisherMemoryOutbox{}
	publisher := testPublisher(t, now, state, func(request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			return publisherHostResponse(request, edgeID), nil
		}
		response, err := apps.Sign("mesh-app/response/v1", state.originID, 2, map[string]string{"requestId": "different"}, edgeKey, now)
		if err != nil {
			return protocol.Control{}, err
		}
		data, err := json.Marshal(response)
		return protocol.Control{Type: protocol.TypeAppEdge, RequestID: request.RequestID, App: data}, err
	})
	publisher.target.Identity = edgeID
	signed, err := apps.Sign("mesh-app/request/v1", edgeID, 1, apps.Request{Action: "list"}, publisher.signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.AppExchange(context.Background(), signed); err == nil {
		t.Fatal("mismatched response sequence accepted")
	}
}

func TestAppAcquireSharesClientAndOriginBudgetsWithServiceProxy(t *testing.T) {
	now := time.Now().UTC()
	registry := testRegistry(t, ModeDirectTLS, now)
	t.Cleanup(registry.Close)
	owner, _ := testIdentity(t)
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "service.shaulavo.dev", ServiceName: "app"},
		Origin: testResolvedOrigin(owner, netip.MustParseAddrPort("127.0.0.1:9"), now),
	}}); err != nil {
		t.Fatal(err)
	}
	var releases []func()
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})
	for index := range maximumConcurrentPerClient {
		release, err := registry.AcquireApp(appBudgetRequest("198.51.100.1"), owner)
		if err != nil {
			t.Fatalf("acquire client slot %d: %v", index, err)
		}
		releases = append(releases, release)
	}
	if release, err := registry.AcquireApp(appBudgetRequest("198.51.100.1"), owner); err == nil {
		release()
		t.Fatal("app bypassed per-client limit")
	}
	releases[0]()
	releases[0]()
	release, err := registry.AcquireApp(appBudgetRequest("198.51.100.1"), owner)
	if err != nil {
		t.Fatalf("release did not restore client capacity: %v", err)
	}
	releases = append(releases, release)
	for index := maximumConcurrentPerClient; index < maximumConcurrentPerOrigin; index++ {
		release, err := registry.AcquireApp(appBudgetRequest(netip.AddrFrom4([4]byte{198, 51, 100, byte(index + 1)}).String()), owner)
		if err != nil {
			t.Fatalf("acquire origin slot %d: %v", index, err)
		}
		releases = append(releases, release)
	}
	if release, err := registry.AcquireApp(appBudgetRequest("203.0.113.1"), owner); err == nil {
		release()
		t.Fatal("app bypassed per-origin limit")
	}
	if len(registry.global) != maximumConcurrentPerOrigin || registry.clients.active[netip.MustParseAddr("203.0.113.1")] != 0 {
		t.Fatal("origin-capacity rejection leaked global or client capacity")
	}
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, publicRequest(http.MethodGet, "service.shaulavo.dev", "/app"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("service did not share saturated origin budget: %d", response.Code)
	}
}

func TestAppGlobalBudgetRejectionRollsBackClientCapacity(t *testing.T) {
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	owner, _ := testIdentity(t)
	for range maximumConcurrentUpstreams {
		registry.global <- struct{}{}
	}
	client := netip.MustParseAddr("198.51.100.1")
	for range maximumConcurrentPerClient + 1 {
		if release, err := registry.AcquireApp(appBudgetRequest(client.String()), owner); err == nil {
			release()
			t.Fatal("app bypassed global budget")
		}
	}
	if registry.clients.active[client] != 0 {
		t.Fatal("global-capacity rejection leaked client capacity")
	}
	<-registry.global
	release, err := registry.AcquireApp(appBudgetRequest(client.String()), owner)
	if err != nil {
		t.Fatalf("released global slot unavailable: %v", err)
	}
	release()
}

func appBudgetRequest(address string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	return r.WithContext(context.WithValue(r.Context(), proxyClientIPKey{}, netip.MustParseAddr(address)))
}

func TestAppAcquireRejectsUnvalidatedClientWithoutBudgetLeak(t *testing.T) {
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	owner, _ := testIdentity(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Forwarded-For", "198.51.100.1")
	if release, err := registry.AcquireApp(request, owner); err == nil {
		release()
		t.Fatal("unvalidated forwarding header became client identity")
	}
	if len(registry.global) != 0 || len(registry.budgets) != 0 {
		t.Fatal("failed admission leaked concurrency budget")
	}
}

func TestCoalescedAppConnectionRequestsRetryBeforeAuthorization(t *testing.T) {
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	calls := 0
	registry.SetAppHandler(appHandlerFunc(func(w http.ResponseWriter, r *http.Request, name string) bool {
		calls++
		w.WriteHeader(http.StatusNoContent)
		return true
	}))
	request := publicRequest(http.MethodGet, apps.ManagementHost, "/frame?id=7k3d")
	request.ProtoMajor = 2
	request.TLS.ServerName = "7k3d.shaulavo.dev"
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest || calls != 0 {
		t.Fatalf("coalesced connection = %d, calls=%d; browser needs a 421 to reconnect", response.Code, calls)
	}
	request.TLS.ServerName = apps.ManagementHost
	response = httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || calls != 1 {
		t.Fatal("dedicated management connection was rejected")
	}
}
