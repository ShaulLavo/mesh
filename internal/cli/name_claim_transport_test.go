package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type namedDestinationFixture struct {
	host          HostRecord
	names         *machinename.Store
	reported      atomic.Pointer[protocol.HostInfo]
	operations    atomic.Int32
	subscriptions chan chan protocol.Control
	stop          chan struct{}
}

func namedDestination(t *testing.T) *namedDestinationFixture {
	t.Helper()
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	return namedDestinationPeer(t)
}

func namedDestinationPeer(t *testing.T) *namedDestinationFixture {
	t.Helper()
	auth, id := controlFixtureAuthentication(t)
	names, err := machinename.Open(t.Context(), t.TempDir(), id, "destination")
	if err != nil {
		t.Fatal(err)
	}
	f := &namedDestinationFixture{
		host:  HostRecord{MachineName: "viewer-label", ID: id, MeshIdentity: id, Addresses: []string{"100.64.0.7"}, TailscaleName: "os-name"},
		names: names, subscriptions: make(chan chan protocol.Control, 4), stop: make(chan struct{}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, f.handle)
	}))
	t.Cleanup(func() { close(f.stop); server.Close() })
	f.host.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/control/ws"
	return f
}

func (f *namedDestinationFixture) info() protocol.HostInfo {
	if reported := f.reported.Load(); reported != nil {
		return *reported
	}
	claim := f.names.Current()
	return protocol.HostInfo{ID: claim.ID, MeshIdentity: claim.ID, MachineName: claim.MachineName, NameRevision: claim.Revision}
}

func (f *namedDestinationFixture) handle(ctx context.Context, conn transport.Conn) error {
	for ctx.Err() == nil {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("name fixture read: %w", err)
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("name fixture decode: %w", err)
		}
		response := protocol.Control{RequestID: request.RequestID}
		switch request.Type {
		case protocol.TypeHostInfo:
			info := f.info()
			response.Type, response.Host = protocol.TypeHostInfoResult, &info
		case protocol.TypeHostRename:
			if request.Rename == nil {
				return fmt.Errorf("missing rename request")
			}
			claim, _, err := f.names.Rename(ctx, request.Rename.TargetID, request.Rename.MachineName, request.Rename.ExpectedRevision)
			if err != nil {
				response.Type = protocol.TypeError
				response.Message = err.Error()
			} else {
				info := f.info()
				info.MachineName, info.NameRevision = claim.MachineName, claim.Revision
				response.Type = protocol.TypeHostRenamed
				response.Host = &info
			}
		case protocol.TypeList:
			f.operations.Add(1)
			response.Type = protocol.TypeListed
		case protocol.TypeStateWatch:
			info := f.info()
			response.Type = protocol.TypeStateSnapshot
			response.StateSnapshot = &protocol.StateSnapshot{Seq: 1, Host: &info, Current: map[string]protocol.Observation{protocol.TopicHost: {}}}
		default:
			return errors.New("unexpected naming fixture control")
		}
		if err := conn.WriteFrame(mustCommandControlFrame(response)); err != nil {
			return fmt.Errorf("name fixture write: %w", err)
		}
		if request.Type == protocol.TypeStateWatch {
			return f.watch(ctx, conn)
		}
	}
	return nil
}

func (f *namedDestinationFixture) watch(ctx context.Context, conn transport.Conn) error {
	events := make(chan protocol.Control, 4)
	select {
	case f.subscriptions <- events:
	case <-ctx.Done():
		return fmt.Errorf("name fixture subscription: %w", ctx.Err())
	case <-f.stop:
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("name fixture watch: %w", ctx.Err())
		case <-f.stop:
			return nil
		case response := <-events:
			if err := conn.WriteFrame(mustCommandControlFrame(response)); err != nil {
				return fmt.Errorf("name fixture event: %w", err)
			}
		}
	}
}

func TestAuthenticatedNameCachePreservesAddressBookAndStopsStaleTarget(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	config, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.Dashboard = &DashboardSettings{Theme: "kanagawa", UsageFeedURL: "https://example.invalid/usage"}
	if err := writeHostConfig(config); err != nil {
		t.Fatal(err)
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // private fixture configuration
	if err != nil {
		t.Fatal(err)
	}
	conn, info, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if info.MachineName != "destination" || info.NameRevision != 1 {
		t.Fatalf("destination claim: %+v", info)
	}
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 1 {
		t.Fatalf("cached address book: %+v %v", hosts, err)
	}
	if hosts[0].MachineName != "destination" || hosts[0].NameRevision != 1 {
		t.Fatal("authenticated name was not cached")
	}
	target, err := ResolveArgument("destination", hosts)
	if err != nil || target.Host == nil {
		t.Fatalf("declared name failed to resolve: %v", err)
	}
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := listRemoteHost(t.Context(), *target.Host, dialControlHost, HostQueryBudget{Setup: remoteConnectTimeout, Reply: defaultCatalogTimeout}); err == nil || f.operations.Load() != 0 {
		t.Fatalf("stale name executed a command: err=%v operations=%d", err, f.operations.Load())
	}
	hosts, err = LoadHosts()
	if err != nil || hosts[0].MachineName != "renamed" || hosts[0].NameRevision != 2 {
		t.Fatalf("reconnect did not refresh claim: %+v %v", hosts, err)
	}
	if _, err := ResolveArgument("destination", hosts); err == nil {
		t.Fatal("retired destination claim still resolved")
	}
	exact, err := ResolveArgument(f.host.ID, hosts)
	if err != nil || exact.Host == nil {
		t.Fatalf("exact destination ID: %v", err)
	}
	conn, _, err = openVerifiedHostInfo(t.Context(), *exact.Host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	after, err := os.ReadFile(path) //nolint:gosec // private fixture configuration
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("name observations changed addresses/settings: %v", err)
	}
	if strings.Contains(string(after), "machineName") {
		t.Fatal("name cache leaked into the address/settings write target")
	}
}

func TestAuthenticatedNameCacheRejectsForgeryReplayAndWrongPin(t *testing.T) {
	f := namedDestination(t)
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	conn, _, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	other, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range []protocol.HostInfo{
		{ID: other.ID, MeshIdentity: f.host.ID, MachineName: "forged", NameRevision: 99},
		{ID: f.host.ID, MeshIdentity: other.ID, MachineName: "forged", NameRevision: 99},
		{ID: f.host.ID, MeshIdentity: f.host.ID, MachineName: "destination", NameRevision: 1},
		{ID: f.host.ID, MeshIdentity: f.host.ID, MachineName: "equivocated", NameRevision: 2},
		{ID: f.host.ID, MeshIdentity: f.host.ID, MachineName: "UPPER", NameRevision: 3},
		{ID: f.host.ID, MeshIdentity: f.host.ID, MachineName: "renamed", NameRevision: 0},
	} {
		f.reported.Store(&info)
		if conn, _, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost); err == nil {
			_ = conn.Close()
			t.Fatalf("invalid authenticated report accepted: %+v", info)
		}
	}
	f.reported.Store(nil)
	wrongPin := f.host
	wrongPin.MeshIdentity = other.ID
	if conn, _, err := openVerifiedHostInfo(t.Context(), wrongPin, dialControlHost); err == nil {
		_ = conn.Close()
		t.Fatal("incorrect destination pin admitted a claim")
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cached, err := machinename.CachedClaim(filepath.Dir(path), f.host.ID)
	if err != nil || cached != f.names.Current() {
		t.Fatalf("rejected claim changed cache: %+v %v", cached, err)
	}
	foreign, err := machinename.CachedClaim(filepath.Dir(path), other.ID)
	if err != nil || foreign.Revision != 0 {
		t.Fatalf("destination cached another peer's assertion: %+v %v", foreign, err)
	}
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	if conn, _, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost); err == nil {
		_ = conn.Close()
		t.Fatal("unapproved device obtained a naming claim")
	}
}

func TestAuthenticatedNameWatchCachesCommittedEventAndReconnect(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ready := make(chan StateView, 2)
	finished := make(chan error, 1)
	go func() {
		finished <- NewStateWatcher(dialControlHost).Watch(ctx, f.host, protocol.StateWatch{Topics: []string{protocol.TopicHost}}, func(view StateView) {
			ready <- view
			if view.Name.Revision == 2 {
				cancel()
			}
		})
	}()
	select {
	case view := <-ready:
		if view.Name.MachineName != "destination" || view.Name.Revision != 1 {
			t.Fatalf("initial authenticated host snapshot: %+v", view.Name)
		}
	case <-ctx.Done():
		t.Fatal("naming watch never published its authenticated snapshot")
	}
	var events chan protocol.Control
	select {
	case events = <-f.subscriptions:
	case <-ctx.Done():
		t.Fatal("naming watch never subscribed")
	}
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	info := f.info()
	events <- protocol.Control{Type: protocol.TypeStateEvent, StateEvent: &protocol.StateEvent{Seq: 2, Kind: "host.changed", Payload: protocol.StatePayload{Host: &info}}}
	select {
	case view := <-ready:
		if view.Name.MachineName != "renamed" || view.Name.Revision != 2 {
			t.Fatalf("committed event: %+v", view.Name)
		}
	case <-ctx.Done():
		select {
		case view := <-ready:
			if view.Name.Revision != 2 {
				t.Fatal("committed event lost")
			}
		default:
			t.Fatal("naming watch never published its committed event")
		}
	}
	<-finished
	hosts, err := LoadHosts()
	if err != nil || hosts[0].MachineName != "renamed" || hosts[0].NameRevision != 2 {
		t.Fatalf("watch cache: %+v %v", hosts, err)
	}
	conn, current, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if current.NameRevision != 2 {
		t.Fatal("reconnect returned an old claim")
	}
}

func TestAuthenticatedNameTargetRechecksNewlyKnownConflict(t *testing.T) {
	f := namedDestination(t)
	peer := namedDestinationPeer(t)
	peer.host.MachineName = "other-viewer-label"
	if _, _, err := peer.names.Rename(t.Context(), peer.host.ID, "other", 1); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []*namedDestinationFixture{f, peer} {
		if err := SaveHost(destination.host); err != nil {
			t.Fatal(err)
		}
		conn, _, err := openVerifiedHostInfo(t.Context(), destination.host, dialControlHost)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	target, err := ResolveArgument("destination", hosts)
	if err != nil || target.Host == nil {
		t.Fatalf("initial unique name: %v", err)
	}
	if _, _, err := peer.names.Rename(t.Context(), peer.host.ID, "destination", 2); err != nil {
		t.Fatal(err)
	}
	conn, _, err := openVerifiedHostInfo(t.Context(), peer.host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if _, err := listRemoteHost(t.Context(), *target.Host, dialControlHost, HostQueryBudget{Setup: remoteConnectTimeout, Reply: defaultCatalogTimeout}); err == nil || f.operations.Load() != 0 {
		t.Fatalf("newly known collision executed a named command: err=%v operations=%d", err, f.operations.Load())
	}
	if _, err := listRemoteHost(t.Context(), f.host, dialControlHost, HostQueryBudget{Setup: remoteConnectTimeout, Reply: defaultCatalogTimeout}); err != nil || f.operations.Load() != 1 {
		t.Fatalf("exact-ID target failed during conflict: err=%v operations=%d", err, f.operations.Load())
	}
}

func TestAuthenticatedHostPollingCachesNamesAndRefusesReplays(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	watcher := NewStateWatcher(dialControlHost)
	view := StateView{Sections: map[string]ObservedSection{}}
	section := pollSection{topic: protocol.TopicHost, build: "null"}
	if pollControl(protocol.TopicHost) != protocol.TypeHostInfo {
		t.Fatal("host poll requested a session list")
	}
	if err := watcher.pollSection(t.Context(), f.host, &section, &view); err != nil {
		t.Fatal(err)
	}
	if view.Name != f.names.Current() || !view.NameVerified {
		t.Fatalf("initial authenticated poll: %+v", view)
	}
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	if err := watcher.pollSection(t.Context(), f.host, &section, &view); err != nil || view.Name != f.names.Current() {
		t.Fatalf("polled rename: %+v %v", view.Name, err)
	}
	for _, info := range []protocol.HostInfo{
		{ID: f.host.ID, MeshIdentity: f.host.ID, MachineName: "destination", NameRevision: 1},
		{ID: f.host.ID, MeshIdentity: f.host.ID, MachineName: "equivocated", NameRevision: 2},
		{ID: "another-owner", MeshIdentity: f.host.ID, MachineName: "forged", NameRevision: 99},
	} {
		f.reported.Store(&info)
		if err := watcher.pollSection(t.Context(), f.host, &section, &view); err == nil || view.Name != f.names.Current() {
			t.Fatalf("invalid poll changed name: %+v %v", view.Name, err)
		}
	}
	legacy := protocol.HostInfo{ID: f.host.ID, MeshIdentity: f.host.ID}
	f.reported.Store(&legacy)
	if err := watcher.pollSection(t.Context(), f.host, &section, &view); err != nil || view.Name != f.names.Current() || view.NameVerified {
		t.Fatalf("legacy poll lost retained claim: %+v %v", view.Name, err)
	}
	hosts, err := LoadHosts()
	if err != nil || hosts[0].MachineName != "renamed" || hosts[0].NameRevision != 2 {
		t.Fatalf("polled cache changed after refusal/legacy response: %+v %v", hosts, err)
	}
}
