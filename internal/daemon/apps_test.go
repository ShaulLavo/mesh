package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
)

type testAppOrigin struct{ calls int }

func (o *testAppOrigin) Handle(_ context.Context, request apps.Request) (apps.Result, error) {
	o.calls++
	return apps.Result{UploadID: request.UploadID}, nil
}

type testAppEdge struct{ calls int }

func (e *testAppEdge) Exchange(_ context.Context, request apps.Signed) (apps.Signed, error) {
	e.calls++
	return request, nil
}

func TestAppOwnerControlRequiresUnixContextBeforeDecodingOrSigning(t *testing.T) {
	origin := &testAppOrigin{}
	controller := &appController{origin: origin}
	request := protocol.Control{Type: protocol.TypeAppRequest, RequestID: "owner", App: json.RawMessage(`{"action":"upload.begin","uploadId":"upload"}`)}
	_, handled, err := controller.HandleControl(context.Background(), request)
	if !handled || err == nil || origin.calls != 0 {
		t.Fatalf("network owner operation: handled=%v error=%v signer calls=%d", handled, err, origin.calls)
	}
	local := context.WithValue(context.Background(), localClientKey{}, true)
	response, handled, err := controller.HandleControl(local, request)
	if !handled || err != nil || origin.calls != 1 || response.Type != protocol.TypeAppResult || response.RequestID != request.RequestID {
		t.Fatalf("local owner operation: response=%+v handled=%v error=%v calls=%d", response, handled, err, origin.calls)
	}
	var result apps.Result
	if err := json.Unmarshal(response.App, &result); err != nil || result.UploadID != "upload" {
		t.Fatalf("local result: %+v, %v", result, err)
	}
}

func TestAppEdgeControlAcceptsSignedTransportWithoutOwnerRPC(t *testing.T) {
	public := &testAppEdge{}
	controller := &appController{edge: public}
	request := protocol.Control{Type: protocol.TypeAppEdge, RequestID: "signed", App: json.RawMessage(`{"domain":"mesh-app/request/v1","owner":"owner"}`)}
	response, handled, err := controller.HandleControl(context.Background(), request)
	if !handled || err != nil || public.calls != 1 || response.Type != protocol.TypeAppEdge {
		t.Fatalf("edge dispatch: response=%+v handled=%v error=%v calls=%d", response, handled, err, public.calls)
	}
	request.Type = protocol.TypeAppRequest
	if _, handled, err := controller.HandleControl(context.Background(), request); !handled || err == nil || public.calls != 1 {
		t.Fatalf("edge exposed owner signing: handled=%v error=%v calls=%d", handled, err, public.calls)
	}
}

func TestAppControlRejectsUnknownAndTrailingFields(t *testing.T) {
	origin := &testAppOrigin{}
	controller := &appController{origin: origin}
	ctx := context.WithValue(context.Background(), localClientKey{}, true)
	for _, raw := range []string{`{"action":"list","secret":true}`, `{"action":"list"}{}`, `null {}`, ""} {
		_, handled, err := controller.HandleControl(ctx, protocol.Control{Type: protocol.TypeAppRequest, App: json.RawMessage(raw)})
		if !handled || err == nil || origin.calls != 0 {
			t.Fatalf("malformed %q: handled=%v error=%v calls=%d", raw, handled, err, origin.calls)
		}
	}
}

func TestAppResolverPinsExactConfiguredIdentityAndNumericEndpoint(t *testing.T) {
	origin := edge.OriginConfig{Identity: "exact-owner", ControlPort: 7337}
	endpoint := netip.MustParseAddrPort("100.64.0.2:7337")
	resolved, pinned := 0, 0
	resolve := func(_ context.Context, got edge.OriginConfig) (netip.AddrPort, error) {
		resolved++
		if got.Identity != origin.Identity {
			t.Fatal("resolved unexpected origin")
		}
		return endpoint, nil
	}
	pin := func(_ context.Context, gotEndpoint netip.AddrPort, got edge.OriginConfig) error {
		pinned++
		if got.Identity != origin.Identity || gotEndpoint != endpoint {
			t.Fatal("pinned unexpected origin")
		}
		return nil
	}
	resolver := appResolver([]edge.OriginConfig{origin}, resolve, pin)
	if _, err := resolver(context.Background(), "other-owner"); err == nil || resolved != 0 || pinned != 0 {
		t.Fatalf("unconfigured origin reached transport: %v, %d, %d", err, resolved, pinned)
	}
	if got, err := resolver(context.Background(), origin.Identity); err != nil || got != endpoint || pinned != 1 {
		t.Fatalf("resolved=%v error=%v pins=%d", got, err, pinned)
	}
	endpoint = netip.MustParseAddrPort("127.0.0.1:7337")
	if _, err := resolver(context.Background(), origin.Identity); err == nil || pinned != 1 {
		t.Fatalf("loopback endpoint reached pin: %v, %d", err, pinned)
	}
	endpoint = netip.MustParseAddrPort("100.64.0.2:22")
	if _, err := resolver(context.Background(), origin.Identity); err == nil || pinned != 1 {
		t.Fatalf("wrong control port reached pin: %v, %d", err, pinned)
	}
	endpoint = netip.MustParseAddrPort("100.64.0.2:7337")
	pinError := errors.New("wrong host identity")
	resolver = appResolver([]edge.OriginConfig{origin}, resolve, func(context.Context, netip.AddrPort, edge.OriginConfig) error { return pinError })
	if _, err := resolver(context.Background(), origin.Identity); !errors.Is(err, pinError) {
		t.Fatalf("pin failure ignored: %v", err)
	}
}

func TestAppDataRootUsesWorkloadStorage(t *testing.T) {
	if got := appDataRoot("/configured/apps", "/state"); got != "/configured/apps" {
		t.Fatalf("explicit app data root=%s", got)
	}
	want := filepath.Join("/state", "apps-data")
	if runtime.GOOS == "linux" {
		want = "/work/mesh/apps"
	}
	if got := appDataRoot("", "/state"); got != want {
		t.Fatalf("default app data root=%s want=%s", got, want)
	}
}

func TestClientServerAppDispatchPreservesUnixAuthorization(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) { testClientServerAppDispatch(t, local) })
	}
}

func testClientServerAppDispatch(t *testing.T, local bool) {
	t.Helper()
	client := newServerTestConn()
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	origin := &testAppOrigin{}
	server.apps = &appController{origin: origin}
	ctx := context.WithValue(context.Background(), localClientKey{}, local)
	done := make(chan error, 1)
	go func() { done <- server.Handle(ctx, client) }()
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeAppRequest, RequestID: "owner-app", App: json.RawMessage(`{"action":"list"}`)}))
	response := decodeServerControl(t, client.nextWrite(t))
	wantType := protocol.TypeError
	if local {
		wantType = protocol.TypeAppResult
	}
	if response.Type != wantType || response.RequestID != "owner-app" || (origin.calls == 1) != local {
		t.Fatalf("local=%v response=%+v origin calls=%d", local, response, origin.calls)
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "app client server"); err != nil {
		t.Fatal(err)
	}
}

type guardedAppWorkers struct{ starts int }

func (w *guardedAppWorkers) Start(context.Context, string, string, string, []string) (string, error) {
	w.starts++
	return "", errors.New("unexpected app launch")
}
func (*guardedAppWorkers) Stop(context.Context, string) error                 { return nil }
func (*guardedAppWorkers) Find(context.Context, string) (string, bool, error) { return "", false, nil }
func (*guardedAppWorkers) Forget(context.Context, string)                     {}

func protectedTestOrigin(t *testing.T, store *storage.Store, registry *meshserve.Registry, root string, activePort int) (*apps.Origin, *guardedAppWorkers) {
	t.Helper()
	if activePort != 0 {
		state := map[string]any{"apps": map[string]any{"7k3d": map[string]any{
			"record": apps.Record{ID: "7k3d", Kind: "server", Status: "active"},
			"root":   filepath.Join(root, "apps", "7k3d", "source"), "command": "server", "port": activePort, "phase": "ready",
		}}, "uploads": map[string]any{}}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveAppState(context.Background(), "apps.origin", data); err != nil {
			t.Fatal(err)
		}
	}
	_, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workers := &guardedAppWorkers{}
	origin, err := apps.NewOrigin(context.Background(), apps.OriginConfig{
		Store: store, Key: key, Workers: workers, DataRoot: root, CheckHosting: checkAppHosting(registry),
		Exchange: func(context.Context, apps.Signed) (apps.Signed, error) {
			return apps.Signed{}, errors.New("unexpected app allocation")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return origin, workers
}

func TestAppCreationRejectsExistingDormantProxyAndListenerAliases(t *testing.T) {
	store, registry, _ := newServiceControllerTest(t, "/mesh")
	service := meshserve.Service{Name: "alias", Kind: meshserve.Proxy, Target: "31337", PublicName: "alias.shaulavo.dev", Listens: []meshserve.Listen{{Public: 31338, Upstream: 31339}}}
	if err := registry.Replace([]meshserve.Service{service}); err != nil {
		t.Fatal(err)
	}
	origin, workers := protectedTestOrigin(t, store, registry, t.TempDir(), 0)
	for _, port := range []int{31337, 31338, 31339} {
		_, err := origin.Handle(context.Background(), apps.Request{Action: "create", Kind: "server", Command: "server", Port: port})
		if err == nil || !strings.Contains(err.Error(), "already exposed by an ordinary service") || workers.starts != 0 {
			t.Fatalf("existing proxy/listen port %d bypassed guard: %v, starts=%d", port, err, workers.starts)
		}
	}
	if err := checkAppHosting(registry)(31340, t.TempDir()); err != nil {
		t.Fatalf("unrelated app port rejected: %v", err)
	}
}

func TestAppHostingCheckRejectsFileRootsAndSymlinkAliases(t *testing.T) {
	store, registry, _ := newServiceControllerTest(t, "/mesh")
	parent := t.TempDir()
	managed := filepath.Join(parent, "managed")
	descendant := filepath.Join(managed, "apps")
	if err := os.MkdirAll(descendant, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	origin, workers := protectedTestOrigin(t, store, registry, managed, 0)
	for _, target := range []string{parent, managed, descendant, alias} {
		if err := registry.Replace([]meshserve.Service{{Name: "files", Kind: meshserve.Files, Target: target}}); err != nil {
			t.Fatal(err)
		}
		_, err := origin.Handle(context.Background(), apps.Request{Action: "upload.begin"})
		if err == nil || !strings.Contains(err.Error(), "overlap an ordinary file service") || workers.starts != 0 {
			t.Fatalf("existing file root %s bypassed app guard: %v", target, err)
		}
	}
	if err := registry.Replace([]meshserve.Service{{Name: "files", Kind: meshserve.Static, Target: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	if err := checkAppHosting(registry)(0, filepath.Join(alias, "new", "missing")); err != nil {
		t.Fatalf("unrelated missing root through alias rejected: %v", err)
	}
}

func TestServiceAppGuardRejectsOwnedPortsAndFileRootsBeforePersistence(t *testing.T) {
	store, registry, controller := newServiceControllerTest(t, "/mesh")
	parent := t.TempDir()
	managed := filepath.Join(parent, "managed")
	descendant := filepath.Join(managed, "apps", "7k3d", "source")
	if err := os.MkdirAll(descendant, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(managed, alias); err != nil {
		t.Fatal(err)
	}
	origin, _ := protectedTestOrigin(t, store, registry, managed, 31337)
	controller.guard = appServiceGuard(origin)
	definitions := []meshserve.Service{
		{Name: "alias", Kind: meshserve.Proxy, Target: "31337", PublicName: "alias.shaulavo.dev"},
		{Name: "alias", Kind: meshserve.Proxy, Target: "31400", Listens: []meshserve.Listen{{Public: 31337, Upstream: 31401}}},
		{Name: "alias", Kind: meshserve.Proxy, Target: "31400", Listens: []meshserve.Listen{{Public: 31401, Upstream: 31337}}},
		{Name: "alias", Kind: meshserve.Files, Target: parent},
		{Name: "alias", Kind: meshserve.Static, Target: managed},
		{Name: "alias", Kind: meshserve.Files, Target: descendant},
		{Name: "alias", Kind: meshserve.Files, Target: alias},
	}
	for index, definition := range definitions {
		info := serviceDefinitionInfo(definition)
		_, err := controller.commitUpsert(context.Background(), protocol.Control{Type: protocol.TypeServiceUpsert, RequestID: "guarded-" + strconv.Itoa(index), Service: &info})
		if err == nil || !strings.Contains(err.Error(), "ordinary services cannot") {
			t.Fatalf("ordinary alias %#v bypassed app guard: %v", definition, err)
		}
		stored, err := store.ListServices(context.Background())
		if err != nil || len(stored) != 0 || len(registry.Services()) != 0 {
			t.Fatalf("rejected alias was persisted or published: %+v, %v", stored, err)
		}
	}
}

func TestServiceAppGuardWaitsForOriginMutationBeforeCheckingOwnedPort(t *testing.T) {
	store, registry, controller := newServiceControllerTest(t, "/mesh")
	origin, _ := protectedTestOrigin(t, store, registry, t.TempDir(), 31337)
	release, err := origin.GuardService(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	guard := appServiceGuard(origin)
	controller.guard = func(ctx context.Context, service meshserve.Service) (func(), error) {
		close(started)
		return guard(ctx, service)
	}
	done := make(chan error, 1)
	go func() {
		_, err := controller.commitUpsert(context.Background(), protocol.Control{Type: protocol.TypeServiceUpsert, RequestID: "concurrent-alias", Service: &protocol.ServiceInfo{Name: "alias", Kind: "proxy", Target: "31337"}})
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		release()
		t.Fatalf("service commit passed the held origin lock: %v", err)
	default:
	}
	release()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "app-owned port") {
		t.Fatalf("service did not inspect owned port after mutation: %v", err)
	}
}
