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
