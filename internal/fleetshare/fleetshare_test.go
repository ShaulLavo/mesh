package fleetshare

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type controllerFixture struct {
	dir     string
	state   string
	host    identity.Host
	key     ed25519.PrivateKey
	service *Service
	server  *httptest.Server
	mu      sync.Mutex
	local   []protocol.FleetHost
}

func newController(t *testing.T) *controllerFixture {
	t.Helper()
	f := &controllerFixture{dir: t.TempDir(), state: t.TempDir()}
	var err error
	f.host, f.key, err = identity.LoadOrCreate(f.state)
	if err != nil {
		t.Fatal(err)
	}
	f.service = &Service{Directory: f.dir, StateDir: f.state, LocalHosts: f.localHosts}
	auth := &transport.Authentication{Key: f.key, Authorize: func(string) bool { return true }}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, f.serve)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *controllerFixture) serve(ctx context.Context, conn transport.Conn) error {
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("read fixture control frame: %w", err)
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode fixture request: %w", err)
	}
	response, handled, err := f.service.HandleControl(ctx, request)
	if !handled || err != nil {
		response = protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: fmt.Sprint(err)}
	}
	payload, err := response.Encode()
	if err != nil {
		return fmt.Errorf("encode fixture response: %w", err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return fmt.Errorf("write fixture response: %w", err)
	}
	return nil
}

func (f *controllerFixture) localHosts(context.Context) ([]protocol.FleetHost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.FleetHost(nil), f.local...), nil
}

func (f *controllerFixture) setLocal(hosts ...protocol.FleetHost) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.local = hosts
}

func (f *controllerFixture) peer() Peer {
	return Peer{Identity: f.host.ID, Endpoint: "ws" + strings.TrimPrefix(f.server.URL, "http") + "/mesh"}
}

func trust(t *testing.T, owner, other *controllerFixture) {
	t.Helper()
	if err := identity.ApproveDevice(owner.state, other.host.ID); err != nil {
		t.Fatal(err)
	}
	if err := Enroll(owner.dir, other.peer()); err != nil {
		t.Fatal(err)
	}
}

func testHost(index int) protocol.FleetHost {
	key := sha256.Sum256([]byte(fmt.Sprint(index)))
	id := base64.RawURLEncoding.EncodeToString(key[:])
	return protocol.FleetHost{ID: id, MeshIdentity: id, Endpoint: "ws://100.64.0.2:7337/mesh", Addresses: []string{"100.64.0.2"}}
}

func requireHosts(t *testing.T, dir string, expected ...protocol.FleetHost) {
	t.Helper()
	hosts, err := Hosts(dir)
	if err != nil || !reflect.DeepEqual(hosts, expected) {
		t.Fatalf("hosts = %#v, error = %v, want %#v", hosts, err, expected)
	}
}

func TestSnapshotsStayLocalAndSurviveOfflineRestarts(t *testing.T) {
	a, b := newController(t), newController(t)
	trust(t, a, b)
	trust(t, b, a)
	x, y := testHost(1), testHost(2)
	a.setLocal(y)
	b.setLocal(x)
	if err := Sync(t.Context(), a.dir, a.state); err != nil {
		t.Fatal(err)
	}
	requireHosts(t, a.dir, x)
	if err := Sync(t.Context(), b.dir, b.state); err != nil {
		t.Fatal(err)
	}
	requireHosts(t, b.dir, y)
	b.server.Close()
	if err := Sync(t.Context(), a.dir, a.state); err == nil {
		t.Fatal("offline controller pull succeeded")
	}
	requireHosts(t, a.dir, x)
	contents, err := os.ReadFile(filepath.Join(a.dir, bookName)) //nolint:gosec // reads only the policy created inside this fixture's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "machineName") {
		t.Fatal("snapshot persisted a remote machine-name assertion")
	}
	freshDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(freshDirectory, bookName), contents, 0o600); err != nil { //nolint:gosec // copies this fixture's policy into another test-owned directory to simulate restart
		t.Fatal(err)
	}
	requireHosts(t, freshDirectory, x)
}

func TestFullSnapshotDeletionAndControllerRevocation(t *testing.T) {
	a, b := newController(t), newController(t)
	trust(t, a, b)
	trust(t, b, a)
	b.setLocal(testHost(1))
	if err := Sync(t.Context(), a.dir, a.state); err != nil {
		t.Fatal(err)
	}
	b.setLocal()
	if err := Sync(t.Context(), a.dir, a.state); err != nil {
		t.Fatal(err)
	}
	requireHosts(t, a.dir)
	b.setLocal(testHost(2))
	if err := Sync(t.Context(), a.dir, a.state); err != nil {
		t.Fatal(err)
	}
	if err := Remove(a.dir, b.host.ID); err != nil {
		t.Fatal(err)
	}
	requireHosts(t, a.dir)
	if !identity.GrantedIdentity(a.state, b.host.ID) {
		t.Fatal("controller removal changed target control approval")
	}
}

func TestFleetRequiresBothControllerConsentAndFullDeviceApproval(t *testing.T) {
	a, b := newController(t), newController(t)
	b.setLocal(testHost(1))
	if _, handled, err := b.service.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeFleetHosts}); !handled || err == nil {
		t.Fatal("unauthenticated local request read the connection book")
	}
	if err := identity.ApproveDevice(b.state, a.host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(t.Context(), a.key, b.peer()); err == nil {
		t.Fatal("approved target device read controller connections without consent")
	}
	if err := Enroll(b.dir, a.peer()); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(t.Context(), a.key, b.peer()); err != nil {
		t.Fatal(err)
	}
	if err := identity.RevokeDevice(b.state, a.host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(t.Context(), a.key, b.peer()); err == nil {
		t.Fatal("authenticated controller without full approval read connections")
	}
}

func TestPullCannotRestoreRemovedOrReenrolledController(t *testing.T) {
	a, b := newController(t), newController(t)
	trust(t, a, b)
	trust(t, b, a)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	b.service.LocalHosts = func(context.Context) ([]protocol.FleetHost, error) {
		close(started)
		<-release
		return []protocol.FleetHost{testHost(1)}, nil
	}
	finished := make(chan error, 1)
	go func() { finished <- Sync(t.Context(), a.dir, a.state) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("pull did not reach controller callback")
	}
	if err := Remove(a.dir, b.host.ID); err != nil {
		t.Fatal(err)
	}
	if err := Enroll(a.dir, b.peer()); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	requireHosts(t, a.dir)
}

func TestRunPullsAtStartupAndRetriesChangedLocalBooks(t *testing.T) {
	a, b := newController(t), newController(t)
	trust(t, a, b)
	trust(t, b, a)
	x := testHost(1)
	b.setLocal(x)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); a.service.run(ctx, 10*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })
	waitHosts(t, a.dir, x.ID)
	b.setLocal(testHost(2))
	waitHosts(t, a.dir, testHost(2).ID)
}

func waitHosts(t *testing.T, dir, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		hosts, err := Hosts(dir)
		if err == nil && len(hosts) == 1 && hosts[0].ID == id {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("controller did not converge on host %s", id)
}

func TestEnrollmentIsIdempotentAndRetainsSnapshot(t *testing.T) {
	a, b := newController(t), newController(t)
	trust(t, a, b)
	trust(t, b, a)
	b.setLocal(testHost(1))
	if err := Sync(t.Context(), a.dir, a.state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(a.dir, bookName)) //nolint:gosec // reads the test-owned policy to compare bytes across repeat enrollment
	if err != nil {
		t.Fatal(err)
	}
	if err := Enroll(a.dir, b.peer()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(a.dir, bookName)) //nolint:gosec // reads the same test-owned policy after repeat enrollment
	if err != nil || string(before) != string(after) {
		t.Fatalf("repeat enrollment changed durable snapshot: %v", err)
	}
	requireHosts(t, a.dir, testHost(1))
}

func TestUnchangedFleetOperationsKeepPublishedFile(t *testing.T) {
	dir := t.TempDir()
	host := testHost(1)
	peer := Peer{Identity: host.ID, Endpoint: host.Endpoint}
	if err := Enroll(dir, peer); err != nil {
		t.Fatal(err)
	}
	state, err := readState(dir)
	if err != nil {
		t.Fatal(err)
	}
	source := state.Peers[0]
	if err := replaceSnapshot(t.Context(), dir, source, []protocol.FleetHost{host}); err != nil {
		t.Fatal(err)
	}
	before := fleetBookInfo(t, dir)
	if err := Enroll(dir, peer); err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, fleetBookInfo(t, dir)) {
		t.Error("identical enrollment replaced the published file")
	}
	if err := replaceSnapshot(t.Context(), dir, source, []protocol.FleetHost{host}); err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, fleetBookInfo(t, dir)) {
		t.Error("identical snapshot replaced the published file")
	}
}

func fleetBookInfo(t *testing.T, dir string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, bookName))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

type snapshotPublisher struct {
	dir       string
	source    enrollment
	publishes int
	fileSyncs int
	dirSyncs  int
	failFile  bool
	failDir   bool
}

func newSnapshotPublisher(t *testing.T) *snapshotPublisher {
	t.Helper()
	host := testHost(1)
	p := &snapshotPublisher{dir: t.TempDir()}
	if err := Enroll(p.dir, Peer{Identity: host.ID, Endpoint: host.Endpoint}); err != nil {
		t.Fatal(err)
	}
	state, err := readState(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	p.source = state.Peers[0]
	return p
}

func (p *snapshotPublisher) publish(dir *os.File, contents []byte) error {
	p.publishes++
	return publishBookWithSync(dir, contents, p.syncFile)
}

func (p *snapshotPublisher) syncFile(file *os.File) error {
	fail := p.failFile
	if file.Name() == p.dir {
		p.dirSyncs++
		fail = p.failDir
	} else {
		p.fileSyncs++
	}
	if fail {
		return unix.EIO
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync fixture publication: %w", err)
	}
	return nil
}

func (p *snapshotPublisher) replace(ctx context.Context, hosts []protocol.FleetHost) error {
	return updateStateWithPublisher(ctx, p.dir, func(state *book) error {
		replaceCurrentSnapshot(state, p.source, hosts)
		return nil
	}, p.publish)
}

func (p *snapshotPublisher) requireCounts(t *testing.T, publishes, fileSyncs, dirSyncs int) {
	t.Helper()
	if p.publishes != publishes || p.fileSyncs != fileSyncs || p.dirSyncs != dirSyncs {
		t.Fatalf("publication/file sync/directory sync counts = %d/%d/%d, want %d/%d/%d", p.publishes, p.fileSyncs, p.dirSyncs, publishes, fileSyncs, dirSyncs)
	}
}

func TestSnapshotPublicationCounts(t *testing.T) {
	p := newSnapshotPublisher(t)
	hosts := []protocol.FleetHost{testHost(1)}
	started := time.Now()
	for range 20 {
		if err := p.replace(t.Context(), hosts); err != nil {
			t.Fatal(err)
		}
	}
	p.requireCounts(t, 1, 1, 1)
	for index := range 20 {
		hosts[0].TailscaleName = fmt.Sprintf("host-%d", index)
		if err := p.replace(t.Context(), hosts); err != nil {
			t.Fatal(err)
		}
	}
	p.requireCounts(t, 21, 21, 21)
	requireHosts(t, p.dir, hosts...)
	t.Logf("20 repeated and 20 changed pulls: 21 publications, 21 file syncs, 21 directory syncs; wall time %s", time.Since(started))
}

func TestSnapshotPublicationFailureRetries(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-rename-%t", afterRename), func(t *testing.T) {
			testSnapshotPublicationFailure(t, afterRename)
		})
	}
}

func testSnapshotPublicationFailure(t *testing.T, afterRename bool) {
	t.Helper()
	p := newSnapshotPublisher(t)
	old, changed := []protocol.FleetHost{testHost(1)}, []protocol.FleetHost{testHost(2)}
	if err := p.replace(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	p.failDir, p.failFile = afterRename, !afterRename
	if err := p.replace(t.Context(), changed); !errors.Is(err, unix.EIO) {
		t.Fatalf("publication failure = %v, want EIO", err)
	}
	visible := old
	if afterRename {
		visible = changed
	}
	requireHosts(t, p.dir, visible...)
	retained, err := readState(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	changed[0].Addresses[0] = "100.64.0.99"
	afterMutation, err := readState(p.dir)
	if err != nil || !reflect.DeepEqual(retained, afterMutation) {
		t.Fatalf("input mutation changed retained snapshot: %v", err)
	}
	changed[0].Addresses[0] = "100.64.0.2"
	retry := &snapshotPublisher{dir: p.dir, source: p.source}
	if err := retry.replace(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	retry.requireCounts(t, 1, 1, 1)
	requireHosts(t, p.dir, changed...)
	if err := retry.replace(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	retry.requireCounts(t, 1, 1, 1)
}

func TestSnapshotEqualityPreservesAllPersistedFields(t *testing.T) {
	cases := []struct {
		name   string
		change func([]protocol.FleetHost)
	}{
		{"identity", func(hosts []protocol.FleetHost) { hosts[0].ID = testHost(3).ID; hosts[0].MeshIdentity = testHost(3).ID }},
		{"endpoint", func(hosts []protocol.FleetHost) { hosts[0].Endpoint = "wss://host.example/mesh" }},
		{"name", func(hosts []protocol.FleetHost) { hosts[0].TailscaleName = "host.example" }},
		{"address", func(hosts []protocol.FleetHost) { hosts[0].Addresses[0] = "100.64.0.99" }},
		{"address-order", func(hosts []protocol.FleetHost) { hosts[0].Addresses = []string{"100.64.0.3", "100.64.0.2"} }},
		{"host-order", func(hosts []protocol.FleetHost) { hosts[0], hosts[1] = hosts[1], hosts[0] }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { testSnapshotFieldChange(t, test.change) })
	}
}

func testSnapshotFieldChange(t *testing.T, change func([]protocol.FleetHost)) {
	t.Helper()
	p := newSnapshotPublisher(t)
	hosts := []protocol.FleetHost{testHost(1), testHost(2)}
	hosts[0].Addresses = []string{"100.64.0.2", "100.64.0.3"}
	if err := p.replace(t.Context(), hosts); err != nil {
		t.Fatal(err)
	}
	change(hosts)
	if err := p.replace(t.Context(), hosts); err != nil {
		t.Fatal(err)
	}
	p.requireCounts(t, 2, 2, 2)
	if err := p.replace(t.Context(), hosts); err != nil {
		t.Fatal(err)
	}
	p.requireCounts(t, 2, 2, 2)
}

func TestEmptyAndStaleSnapshotsDoNotPublish(t *testing.T) {
	p := newSnapshotPublisher(t)
	if err := p.replace(t.Context(), []protocol.FleetHost{}); err != nil {
		t.Fatal(err)
	}
	host := testHost(1)
	host.Addresses = nil
	if err := p.replace(t.Context(), []protocol.FleetHost{host}); err != nil {
		t.Fatal(err)
	}
	host.Addresses = []string{}
	if err := p.replace(t.Context(), []protocol.FleetHost{host}); err != nil {
		t.Fatal(err)
	}
	p.requireCounts(t, 1, 1, 1)
	if err := p.replace(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := p.replace(t.Context(), []protocol.FleetHost{}); err != nil {
		t.Fatal(err)
	}
	p.source.Generation = strings.Repeat("f", 32)
	if err := p.replace(t.Context(), []protocol.FleetHost{testHost(2)}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p.dir, p.source.Identity); err != nil {
		t.Fatal(err)
	}
	if err := p.replace(t.Context(), []protocol.FleetHost{testHost(2)}); err != nil {
		t.Fatal(err)
	}
	p.requireCounts(t, 2, 2, 2)
	requireHosts(t, p.dir)
}

func TestValidationRejectsChangedPinsDuplicatesAndBounds(t *testing.T) {
	valid := testHost(1)
	changed := valid
	changed.MeshIdentity = testHost(2).ID
	unsafeEndpoint := valid
	unsafeEndpoint.Endpoint = "ws://user:password@100.64.0.2:7337/mesh"
	for _, hosts := range [][]protocol.FleetHost{{changed}, {unsafeEndpoint}, {valid, valid}, make([]protocol.FleetHost, maximumEntries+1)} {
		if err := validateHosts(hosts); err == nil {
			t.Fatalf("accepted invalid hosts %#v", hosts)
		}
	}
	entries := make([]enrollment, maximumEntries+1)
	if err := validateBook(&book{Version: 1, Peers: entries}); err == nil {
		t.Fatal("accepted more than 256 controllers")
	}
	first := enrollment{Peer: Peer{Identity: valid.ID, Endpoint: valid.Endpoint}, Generation: strings.Repeat("0", 32)}
	if err := validateBook(&book{Version: 1, Peers: []enrollment{first, first}}); err == nil {
		t.Fatal("accepted duplicate controller")
	}
	state := book{Version: 1, Peers: []enrollment{first}}
	state.Peers[0].Hosts = make([]protocol.FleetHost, maximumEntries)
	for index := range state.Peers[0].Hosts {
		state.Peers[0].Hosts[index] = testHost(index)
	}
	second := first
	second.Identity = testHost(1000).ID
	second.Hosts = []protocol.FleetHost{testHost(1001)}
	state.Peers = append(state.Peers, second)
	if err := validateBook(&state); err == nil {
		t.Fatal("accepted more than 256 unique imported hosts")
	}
}

func TestPrivateStorageRejectsSymlinksAndUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	peer := Peer{Identity: testHost(1).ID, Endpoint: testHost(1).Endpoint}
	if err := Enroll(dir, peer); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, bookName)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("fleet book permissions %v, %v", info, err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // intentionally makes this test policy unsafe to verify that reads reject it
		t.Fatal(err)
	}
	if _, err := Hosts(dir); err == nil {
		t.Fatal("accepted shared-readable fleet policy")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, ".connection-sharing.lock"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Peers(dir); err == nil {
		t.Fatal("followed fleet book symlink")
	}
}

func TestMalformedPolicyDoesNotGrantControllerAccess(t *testing.T) {
	for _, contents := range []string{`{"version":1,"version":1,"peers":[]}`, `{"version":1,"Version":1,"peers":[]}`, `{"version":1,"peers":[],"unknown":true}`, `{"version":1,"peers":[]} {}`} {
		if _, err := decodeBook([]byte(contents)); err == nil {
			t.Fatalf("accepted malformed configuration %s", contents)
		}
	}
	valid := book{Version: 1}
	contents, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeBook(contents); err != nil {
		t.Fatal(err)
	}
}

func TestReadingUnconfiguredSharingLeavesNoFiles(t *testing.T) {
	dir := t.TempDir()
	updater := filepath.Join(dir, "fleet.json")
	contents := []byte(`{"updates":"separate policy"}`)
	if err := os.WriteFile(updater, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Peers(dir); err != nil {
		t.Fatal(err)
	}
	requireHosts(t, dir)
	if err := Sync(t.Context(), dir, filepath.Join(dir, "absent-state")); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != "fleet.json" {
		t.Fatalf("reading sharing created files: %v, %v", files, err)
	}
	after, err := os.ReadFile(updater) //nolint:gosec // reads the separate updater fixture to prove sharing left it untouched
	if err != nil || string(after) != string(contents) {
		t.Fatalf("reading sharing changed updater policy: %q, %v", after, err)
	}
}

func TestConcurrentEnrollmentPreservesEveryController(t *testing.T) {
	dir := t.TempDir()
	var workers sync.WaitGroup
	errors := make(chan error, 16)
	for index := range 16 {
		workers.Go(func() {
			host := testHost(index)
			errors <- Enroll(dir, Peer{Identity: host.ID, Endpoint: host.Endpoint})
		})
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	peers, err := Peers(dir)
	if err != nil || len(peers) != 16 {
		t.Fatalf("concurrent enrollment lost peers: %d, %v", len(peers), err)
	}
}

func TestConcurrentSyncKeepsNewestSnapshotWhenSourceGoesOffline(t *testing.T) {
	a, b := newController(t), newController(t)
	trust(t, a, b)
	trust(t, b, a)
	old, newest := testHost(1), testHost(2)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	b.service.LocalHosts = func(context.Context) ([]protocol.FleetHost, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return []protocol.FleetHost{old}, nil
		}
		return []protocol.FleetHost{newest}, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var workers sync.WaitGroup
	results := make(chan error, 2)
	t.Cleanup(func() { unblock(); cancel(); workers.Wait() })
	workers.Go(func() { results <- Sync(ctx, a.dir, a.state) })
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first sync did not capture the old source snapshot")
	}
	requireSyncLockHeld(t, a.dir)
	workers.Go(func() { results <- Sync(ctx, a.dir, a.state) })
	unblock()
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("source received %d pulls, want 2", calls.Load())
	}
	requireHosts(t, a.dir, newest)
	b.server.Close()
	if err := Sync(ctx, a.dir, a.state); err == nil {
		t.Fatal("offline source pull succeeded")
	}
	requireHosts(t, a.dir, newest)
}

func requireSyncLockHeld(t *testing.T, directory string) {
	t.Helper()
	dir, err := openDirectory(directory, false)
	if err != nil || dir == nil {
		t.Fatalf("open sync directory: %v", err)
	}
	defer func() { _ = dir.Close() }()
	lock, err := openFile(dir, ".connection-sharing-sync.lock", unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		t.Fatal("sync released its cross-process lock during the source request")
	}
	if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("inspect held sync lock: %v", err)
	}
}

func TestWaitingSyncHonorsCallerDeadline(t *testing.T) {
	a, b := newController(t), newController(t)
	trust(t, a, b)
	dir, err := openDirectory(a.dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	lock, err := openFile(dir, ".connection-sharing-sync.lock", unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := Sync(ctx, a.dir, a.state); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked sync returned %v, want caller deadline", err)
	}
}
