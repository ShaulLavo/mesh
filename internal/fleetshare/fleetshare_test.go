package fleetshare

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	if err := identity.ApproveDevice(t.Context(), owner.state, other.host.ID); err != nil {
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
	if err := identity.ApproveDevice(t.Context(), b.state, a.host.ID); err != nil {
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
	if err := identity.RevokeDevice(t.Context(), b.state, a.host.ID); err != nil {
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
	lockSyncs int
	failFile  bool
	failDir   bool
	failReset bool
	failLock  bool
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

func (p *snapshotPublisher) publish(dir, lock *os.File, contents []byte) error {
	if err := invalidateAcknowledgement(snapshotPublicationLock{File: lock, publisher: p}); err != nil {
		return err
	}
	p.publishes++
	return publishBookWithSync(dir, contents, p.syncFile)
}

type snapshotPublicationLock struct {
	*os.File
	publisher *snapshotPublisher
}

func (lock snapshotPublicationLock) Truncate(size int64) error {
	if lock.publisher.failReset {
		return unix.EIO
	}
	if err := lock.File.Truncate(size); err != nil {
		return fmt.Errorf("truncate fixture acknowledgement: %w", err)
	}
	return nil
}

func (lock snapshotPublicationLock) Sync() error {
	lock.publisher.lockSyncs++
	if lock.publisher.failLock {
		return unix.EIO
	}
	if err := lock.File.Sync(); err != nil {
		return fmt.Errorf("sync fixture acknowledgement: %w", err)
	}
	return nil
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
	if p.publishes != publishes || p.fileSyncs != fileSyncs || p.dirSyncs != dirSyncs || p.lockSyncs != publishes {
		t.Fatalf("publication/file sync/directory sync/lock sync counts = %d/%d/%d/%d, want %d/%d/%d/%d", p.publishes, p.fileSyncs, p.dirSyncs, p.lockSyncs, publishes, fileSyncs, dirSyncs, publishes)
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
	t.Logf("20 repeated and 20 changed pulls: 21 publications, 21 file syncs, 21 directory syncs, 21 lock syncs; wall time %s", time.Since(started))
}

func TestSnapshotPublicationFailureRetries(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-rename-%t", afterRename), func(t *testing.T) {
			testSnapshotPublicationFailure(t, afterRename)
		})
	}
}

func TestSnapshotAcknowledgementCannotSurviveFailedStateCycle(t *testing.T) {
	p := newSnapshotPublisher(t)
	a, b := []protocol.FleetHost{testHost(1)}, []protocol.FleetHost{testHost(2)}
	if err := p.replace(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	p.failDir = true
	for _, hosts := range [][]protocol.FleetHost{b, a} {
		if err := p.replace(t.Context(), hosts); !errors.Is(err, unix.EIO) {
			t.Fatalf("state-cycle publication failure = %v, want EIO", err)
		}
	}
	requireHosts(t, p.dir, a...)
	retry := &snapshotPublisher{dir: p.dir, source: p.source, failDir: true}
	if err := retry.replace(t.Context(), a); !errors.Is(err, unix.EIO) {
		t.Fatalf("identical A retry after failed B/A cycle = %v, want EIO", err)
	}
	retry.requireCounts(t, 1, 1, 1)
	durable := &snapshotPublisher{dir: p.dir, source: p.source}
	if err := durable.replace(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	durable.requireCounts(t, 1, 1, 1)
	if err := durable.replace(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	durable.requireCounts(t, 1, 1, 1)
}

func TestSnapshotInvalidationErrorsAbortBeforePublication(t *testing.T) {
	for _, syncFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("sync-failure-%t", syncFailure), func(t *testing.T) {
			testSnapshotInvalidationFailure(t, syncFailure)
		})
	}
}

func testSnapshotInvalidationFailure(t *testing.T, syncFailure bool) {
	t.Helper()
	p := newSnapshotPublisher(t)
	a, b := []protocol.FleetHost{testHost(1)}, []protocol.FleetHost{testHost(2)}
	if err := p.replace(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	before := fleetBookInfo(t, p.dir)
	p.failReset, p.failLock = !syncFailure, syncFailure
	for range 2 {
		if err := p.replace(t.Context(), b); !errors.Is(err, unix.EIO) {
			t.Fatalf("invalidation failure = %v, want EIO", err)
		}
	}
	requireHosts(t, p.dir, a...)
	if !os.SameFile(before, fleetBookInfo(t, p.dir)) {
		t.Fatal("invalidation error allowed a rename")
	}
	wantLockSyncs := 1
	if syncFailure {
		wantLockSyncs = 3
	}
	if p.publishes != 1 || p.fileSyncs != 1 || p.dirSyncs != 1 || p.lockSyncs != wantLockSyncs {
		t.Fatalf("invalidation error reached publication: %#v", p)
	}
	retry := &snapshotPublisher{dir: p.dir, source: p.source}
	if err := retry.replace(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	retry.requireCounts(t, 1, 1, 1)
	if err := retry.replace(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	retry.requireCounts(t, 1, 1, 1)
	requireHosts(t, p.dir, b...)
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
	if err := p.replace(t.Context(), changed); !errors.Is(err, unix.EIO) {
		t.Fatalf("repeated publication failure = %v, want EIO", err)
	}
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

func TestMissingOrMalformedAcknowledgementRetries(t *testing.T) {
	for _, acknowledgement := range []string{"", "short", strings.Repeat("x", 32), strings.Repeat("x", 33)} {
		t.Run(fmt.Sprintf("length-%d", len(acknowledgement)), func(t *testing.T) {
			testMalformedAcknowledgement(t, acknowledgement)
		})
	}
}

func testMalformedAcknowledgement(t *testing.T, acknowledgement string) {
	t.Helper()
	p := newSnapshotPublisher(t)
	hosts := []protocol.FleetHost{testHost(1)}
	if err := p.replace(t.Context(), hosts); err != nil {
		t.Fatal(err)
	}
	lock := openSnapshotLock(t, p.dir)
	if err := lock.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.WriteAt([]byte(acknowledgement), 0); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	retry := &snapshotPublisher{dir: p.dir, source: p.source}
	if err := retry.replace(t.Context(), hosts); err != nil {
		t.Fatal(err)
	}
	retry.requireCounts(t, 1, 1, 1)
	if err := retry.replace(t.Context(), hosts); err != nil {
		t.Fatal(err)
	}
	retry.requireCounts(t, 1, 1, 1)
}

func openSnapshotLock(t *testing.T, path string) *os.File {
	t.Helper()
	dir, err := openDirectory(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	lock, err := openFile(dir, ".connection-sharing.lock", unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}

type failedAcknowledgementWriter struct {
	*os.File
	err error
}

func (w failedAcknowledgementWriter) WriteAt(contents []byte, offset int64) (int, error) {
	n, err := w.File.WriteAt(contents[:len(contents)/2], offset)
	if err != nil {
		return n, fmt.Errorf("write partial fixture acknowledgement: %w", err)
	}
	return n, w.err
}

func TestFailedAcknowledgementWritesRetry(t *testing.T) {
	for _, failure := range []error{nil, unix.EIO} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) { testFailedAcknowledgementWrite(t, failure) })
	}
}

func testFailedAcknowledgementWrite(t *testing.T, failure error) {
	t.Helper()
	p := newSnapshotPublisher(t)
	hosts := []protocol.FleetHost{testHost(2)}
	state := book{Version: 1, Peers: []enrollment{p.source}}
	state.Peers[0].Hosts = hosts
	contents, err := encodeBook(state)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := openDirectory(p.dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	lock := openSnapshotLock(t, p.dir)
	if err := acquireLockFor(t.Context(), lock, lockLimit); err != nil {
		t.Fatal(err)
	}
	if err := publishBook(dir, lock, contents); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	want := failure
	if want == nil {
		want = io.ErrShortWrite
	}
	if err := acknowledgeBook(failedAcknowledgementWriter{File: lock, err: failure}, digest[:]); !errors.Is(err, want) {
		t.Fatalf("acknowledgement error = %v, want %v", err, want)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	requireHosts(t, p.dir, hosts...)
	retry := &snapshotPublisher{dir: p.dir, source: p.source}
	if err := retry.replace(t.Context(), hosts); err != nil {
		t.Fatal(err)
	}
	retry.requireCounts(t, 1, 1, 1)
}

func TestBookEqualityTracksGenerationAndInPlaceChanges(t *testing.T) {
	p := newSnapshotPublisher(t)
	if err := p.replace(t.Context(), []protocol.FleetHost{testHost(1)}); err != nil {
		t.Fatal(err)
	}
	err := updateStateWithPublisher(t.Context(), p.dir, func(state *book) error {
		state.Peers[0].Generation = strings.Repeat("f", 32)
		state.Peers[0].Endpoint = "wss://controller.example/mesh"
		state.Peers[0].Hosts[0].Addresses[0] = "100.64.0.99"
		return nil
	}, p.publish)
	if err != nil {
		t.Fatal(err)
	}
	p.requireCounts(t, 2, 2, 2)
	if err := p.replace(t.Context(), []protocol.FleetHost{testHost(2)}); err != nil {
		t.Fatal(err)
	}
	p.requireCounts(t, 2, 2, 2)
	state, err := readState(p.dir)
	if err != nil || state.Peers[0].Generation != strings.Repeat("f", 32) || state.Peers[0].Endpoint != "wss://controller.example/mesh" || state.Peers[0].Hosts[0].Addresses[0] != "100.64.0.99" {
		t.Fatalf("in-place changes were lost: %#v, %v", state, err)
	}
}

func TestConcurrentIdenticalSnapshotsPublishOnce(t *testing.T) {
	p := newSnapshotPublisher(t)
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		workers.Go(func() { results <- p.replace(t.Context(), []protocol.FleetHost{testHost(1)}) })
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	p.requireCounts(t, 1, 1, 1)
	requireHosts(t, p.dir, testHost(1))
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

type publicationGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *publicationGate) open() { g.once.Do(func() { close(g.release) }) }

type queuedPublisher struct {
	dir         string
	calls       atomic.Int32
	locks       atomic.Int32
	files       atomic.Int32
	dirs        atomic.Int32
	gates       map[int32]*publicationGate
	failAt      int32
	afterRename bool
	workers     sync.WaitGroup
}

func newQueuedPublisher(t *testing.T, pauses ...int32) *queuedPublisher {
	t.Helper()
	p := &queuedPublisher{dir: t.TempDir(), gates: make(map[int32]*publicationGate)}
	for _, call := range pauses {
		p.gates[call] = &publicationGate{entered: make(chan struct{}), release: make(chan struct{})}
	}
	t.Cleanup(func() {
		for _, gate := range p.gates {
			gate.open()
		}
		p.workers.Wait()
	})
	return p
}

func (p *queuedPublisher) publish(dir, lock *os.File, contents []byte) error {
	call := p.calls.Add(1)
	if gate := p.gates[call]; gate != nil {
		close(gate.entered)
		<-gate.release
	}
	if err := invalidateAcknowledgement(queuedPublicationLock{File: lock, calls: &p.locks}); err != nil {
		return err
	}
	return publishBookWithSync(dir, contents, func(file *os.File) error {
		isDir := file == dir
		if isDir {
			p.dirs.Add(1)
		} else {
			p.files.Add(1)
		}
		if call == p.failAt && isDir == p.afterRename {
			return unix.EIO
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync queued fixture: %w", err)
		}
		return nil
	})
}

type queuedPublicationLock struct {
	*os.File
	calls *atomic.Int32
}

func (lock queuedPublicationLock) Sync() error {
	lock.calls.Add(1)
	if err := lock.File.Sync(); err != nil {
		return fmt.Errorf("sync queued invalidation: %w", err)
	}
	return nil
}

func (p *queuedPublisher) start(ctx context.Context, operation func(*book) error) <-chan error {
	result := make(chan error, 1)
	p.workers.Go(func() { result <- updateStateWithPublisher(ctx, p.dir, operation, p.publish) })
	return result
}

func queueEnrollment(index int) func(*book) error {
	host := testHost(index)
	return func(state *book) error {
		return enroll(state, enrollment{Peer: Peer{Identity: host.ID, Endpoint: host.Endpoint}, Generation: strings.Repeat("0", 32)})
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture signal timed out")
	}
}

func queuedResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("queued result timed out")
		return nil
	}
}

func pendingMutations(t *testing.T, configDir string) int {
	t.Helper()
	dir, err := openDirectory(configDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	c, leave, err := joinCommitter(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func waitPending(t *testing.T, configDir string, want int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for pendingMutations(t, configDir) != want {
		select {
		case <-deadline.C:
			t.Fatalf("pending mutations = %d, want %d", pendingMutations(t, configDir), want)
		case <-ticker.C:
		}
	}
}

func (p *queuedPublisher) requireCounts(t *testing.T, want int32) {
	t.Helper()
	if p.calls.Load() != want || p.locks.Load() != want || p.files.Load() != want || p.dirs.Load() != want {
		t.Fatalf("publication/lock/file/directory counts = %d/%d/%d/%d, want %d each", p.calls.Load(), p.locks.Load(), p.files.Load(), p.dirs.Load(), want)
	}
}

func TestQueuedDistinctEnrollmentsShareOnePublication(t *testing.T) {
	p := newQueuedPublisher(t, 1)
	results := []<-chan error{p.start(t.Context(), queueEnrollment(0))}
	waitSignal(t, p.gates[1].entered)
	for i := 1; i < 16; i++ {
		results = append(results, p.start(t.Context(), queueEnrollment(i)))
	}
	waitPending(t, p.dir, 15)
	p.gates[1].open()
	for _, result := range results {
		if err := queuedResult(t, result); err != nil {
			t.Fatal(err)
		}
	}
	peers, err := Peers(p.dir)
	if err != nil || len(peers) != 16 {
		t.Fatalf("peers = %d, %v", len(peers), err)
	}
	p.requireCounts(t, 2)
}

func TestAcceptedMutationIgnoresLaterCancellation(t *testing.T) {
	p := newQueuedPublisher(t, 1, 2)
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := p.start(ctx, queueEnrollment(1))
	c := p.start(ctx, queueEnrollment(2))
	waitPending(t, p.dir, 2)
	p.gates[1].open()
	waitSignal(t, p.gates[2].entered)
	cancel()
	p.gates[2].open()
	for _, result := range []<-chan error{holder, b, c} {
		if err := queuedResult(t, result); err != nil {
			t.Fatal(err)
		}
	}
	p.requireCounts(t, 2)
}

func TestBatchRejectsOnlyFailingMutations(t *testing.T) {
	p := newQueuedPublisher(t, 1)
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	sentinel := errors.New("rejected operation")
	operations := []func(*book) error{
		queueEnrollment(1),
		func(state *book) error { state.Version = 7; return sentinel },
		func(state *book) error { state.Peers = append(state.Peers, state.Peers[0]); return nil },
		queueEnrollment(2),
	}
	var results []<-chan error
	for i, operation := range operations {
		results = append(results, p.start(t.Context(), operation))
		waitPending(t, p.dir, i+1)
	}
	p.gates[1].open()
	if err := queuedResult(t, holder); err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		err := queuedResult(t, result)
		if i == 1 && !errors.Is(err, sentinel) {
			t.Fatalf("own rejection = %v", err)
		}
		if i == 2 && (err == nil || !strings.Contains(err.Error(), "duplicate controller")) {
			t.Fatalf("validation rejection = %v", err)
		}
		if (i == 0 || i == 3) && err != nil {
			t.Fatal(err)
		}
	}
	peers, err := Peers(p.dir)
	if err != nil || len(peers) != 3 {
		t.Fatalf("peers = %d, %v", len(peers), err)
	}
	p.requireCounts(t, 2)
}

func TestBatchKeepsRevocationOrder(t *testing.T) {
	for _, order := range []string{"remove-snapshot", "snapshot-remove", "reenroll-snapshot"} {
		t.Run(order, func(t *testing.T) { testBatchRevocation(t, order) })
	}
}

func testBatchRevocation(t *testing.T, order string) {
	t.Helper()
	p := newQueuedPublisher(t, 1)
	host := testHost(1)
	if err := Enroll(p.dir, Peer{Identity: host.ID, Endpoint: host.Endpoint}); err != nil {
		t.Fatal(err)
	}
	state, err := readState(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	source := state.Peers[0]
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	revoke := func(state *book) error { remove(state, source.Identity); return nil }
	snapshot := func(state *book) error {
		replaceCurrentSnapshot(state, source, []protocol.FleetHost{testHost(2)})
		return nil
	}
	operations := []func(*book) error{revoke, snapshot}
	if order == "snapshot-remove" {
		operations = []func(*book) error{snapshot, revoke}
	}
	if order == "reenroll-snapshot" {
		operations[0] = func(state *book) error {
			next := source
			next.Generation = strings.Repeat("f", 32)
			next.Endpoint = "ws://100.64.0.3:7337/mesh"
			return enroll(state, next)
		}
	}
	results := []<-chan error{holder}
	for i, op := range operations {
		results = append(results, p.start(t.Context(), op))
		waitPending(t, p.dir, i+1)
	}
	p.gates[1].open()
	for _, result := range results {
		if err := queuedResult(t, result); err != nil {
			t.Fatal(err)
		}
	}
	requireHosts(t, p.dir)
	p.requireCounts(t, 2)
}

func TestBatchStateCycleUsesAcknowledgement(t *testing.T) {
	for _, unacknowledged := range []bool{false, true} {
		t.Run(fmt.Sprint(unacknowledged), func(t *testing.T) { testBatchStateCycle(t, unacknowledged) })
	}
}

func testBatchStateCycle(t *testing.T, unacknowledged bool) {
	t.Helper()
	p := newQueuedPublisher(t, 1)
	if err := updateState(t.Context(), p.dir, queueEnrollment(1)); err != nil {
		t.Fatal(err)
	}
	if unacknowledged {
		p.failAt = 1
		p.afterRename = true
	}
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	b := p.start(t.Context(), func(state *book) error { state.Peers[0].Hosts = []protocol.FleetHost{testHost(3)}; return nil })
	waitPending(t, p.dir, 1)
	a := p.start(t.Context(), func(state *book) error { state.Peers[0].Hosts = nil; return nil })
	waitPending(t, p.dir, 2)
	p.gates[1].open()
	if err := queuedResult(t, holder); errors.Is(err, unix.EIO) != unacknowledged {
		t.Fatalf("holder = %v", err)
	}
	for _, result := range []<-chan error{b, a} {
		if err := queuedResult(t, result); err != nil {
			t.Fatal(err)
		}
	}
	want := int32(1)
	if unacknowledged {
		want = 2
	}
	if p.calls.Load() != want {
		t.Fatalf("publications = %d, want %d", p.calls.Load(), want)
	}
	if !unacknowledged {
		p.requireCounts(t, 1)
	}
	requireHosts(t, p.dir)
}

func TestBatchPublicationFailureReachesEveryApplied(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(fmt.Sprint(afterRename), func(t *testing.T) { testBatchPublicationFailure(t, afterRename) })
	}
}

func testBatchPublicationFailure(t *testing.T, afterRename bool) {
	t.Helper()
	p := newQueuedPublisher(t, 1)
	p.failAt, p.afterRename = 2, afterRename
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	noop := func(*book) error { return nil }
	sentinel := errors.New("own rejection")
	operations := []func(*book) error{noop, queueEnrollment(1), func(*book) error { return sentinel }, noop}
	var results []<-chan error
	for i, op := range operations {
		results = append(results, p.start(t.Context(), op))
		waitPending(t, p.dir, i+1)
	}
	p.gates[1].open()
	if err := queuedResult(t, holder); err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		want := error(unix.EIO)
		if i == 2 {
			want = sentinel
		}
		if err := queuedResult(t, result); !errors.Is(err, want) {
			t.Fatalf("outcome %d = %v, want %v", i, err, want)
		}
	}
	state, err := readState(p.dir)
	wantPeers := 1
	if afterRename {
		wantPeers = 2
	}
	if err != nil || len(state.Peers) != wantPeers {
		t.Fatalf("visible peers = %d, %v", len(state.Peers), err)
	}
	p.requireFailedCounts(t, afterRename)
	if err := queuedResult(t, p.start(t.Context(), queueEnrollment(1))); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 3 || p.locks.Load() != 3 || p.files.Load() != 3 {
		t.Fatal("retry skipped publication barriers")
	}
}

func (p *queuedPublisher) requireFailedCounts(t *testing.T, afterRename bool) {
	t.Helper()
	wantDirs := int32(1)
	if afterRename {
		wantDirs = 2
	}
	if p.calls.Load() != 2 || p.locks.Load() != 2 || p.files.Load() != 2 || p.dirs.Load() != wantDirs {
		t.Fatal("failed batch did not retain expected barriers")
	}
}

func TestWaitingMutationHonorsCallerDeadline(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) { testWaitingMutation(t, canceled) })
	}
}

func testWaitingMutation(t *testing.T, canceled bool) {
	t.Helper()
	p := newQueuedPublisher(t, 1)
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var results []<-chan error
	for i := 1; i < 16; i++ {
		results = append(results, p.start(ctx, queueEnrollment(i)))
	}
	if canceled {
		waitPending(t, p.dir, 15)
		cancel()
	}
	want := context.DeadlineExceeded
	if canceled {
		want = context.Canceled
	}
	for _, result := range results {
		if err := queuedResult(t, result); !errors.Is(err, want) || !strings.Contains(err.Error(), "fleet: acquire configuration lock") {
			t.Fatalf("withdrawal = %v", err)
		}
	}
	waitPending(t, p.dir, 0)
	p.gates[1].open()
	if err := queuedResult(t, holder); err != nil {
		t.Fatal(err)
	}
	if err := queuedResult(t, p.start(t.Context(), queueEnrollment(99))); err != nil {
		t.Fatal(err)
	}
	peers, err := Peers(p.dir)
	if err != nil || len(peers) != 2 {
		t.Fatalf("withdrawn changes persisted: %d, %v", len(peers), err)
	}
}

func TestSeparateProcessWriterHelper(t *testing.T) {
	if os.Getenv("MESH_GROUP_COMMIT_CHILD") == "" {
		t.Skip("child process fixture")
	}
	dir := "."
	lock := openSnapshotLock(t, dir)
	locked, err := tryLock(lock)
	if err != nil || locked {
		t.Fatalf("child did not contend on actual flock: %v, %v", locked, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contended"), []byte("flock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updateState(t.Context(), dir, queueEnrollment(1)); err != nil {
		t.Fatal(err)
	}
}

func TestSeparateProcessWritersBothPersist(t *testing.T) {
	p := newQueuedPublisher(t, 1)
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestSeparateProcessWriterHelper$", "-test.v") //nolint:gosec // Re-executes this test binary, whose path comes from os.Executable.
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "MESH_GROUP_COMMIT_CHILD=1")
	output := new(strings.Builder)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	t.Cleanup(func() {
		p.gates[1].open()
		if !joined {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(filepath.Join(p.dir, "contended")); err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("child did not report actual flock contention")
		case <-time.After(time.Millisecond):
		}
	}
	p.gates[1].open()
	err = cmd.Wait()
	joined = true
	if err != nil {
		t.Fatalf("child: %v: %s", err, output.String())
	}
	if err := queuedResult(t, holder); err != nil {
		t.Fatal(err)
	}
	peers, err := Peers(p.dir)
	if err != nil || len(peers) != 2 {
		t.Fatalf("cross-process merge: %d, %v", len(peers), err)
	}
}

func TestPickupWithdrawalRaceExecutesAtMostOnce(t *testing.T) {
	for _, publicationError := range []error{nil, unix.EIO} {
		for range 32 {
			testPickupWithdrawalRace(t, publicationError)
		}
	}
}

func testPickupWithdrawalRace(t *testing.T, publicationError error) {
	t.Helper()
	c := &committer{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var executed atomic.Int32
	m := c.submit(ctx, func(*book) error { executed.Add(1); return nil })
	start := make(chan struct{})
	accepted := make(chan []*mutation, 1)
	withdrawn := make(chan bool, 1)
	var workers sync.WaitGroup
	workers.Go(func() { <-start; accepted <- c.accept(m) })
	workers.Go(func() { <-start; cancel(); withdrawn <- c.withdraw(m) })
	close(start)
	workers.Wait()
	batch, removed := <-accepted, <-withdrawn
	if removed {
		if len(batch) != 0 || executed.Load() != 0 {
			t.Fatal("withdrawn operation accepted")
		}
		return
	}
	if len(batch) != 1 || batch[0] != m {
		t.Fatal("accepted membership lost")
	}
	if err := m.operation(&book{}); err != nil {
		t.Fatal(err)
	}
	m.outcome <- publicationError
	if err := queuedResult(t, m.outcome); !errors.Is(err, publicationError) || executed.Load() != 1 {
		t.Fatalf("accepted outcome = %v, executions %d", err, executed.Load())
	}
	if len(m.outcome) != 0 {
		t.Fatal("duplicate result")
	}
}

func TestAcceptedLeaderAndFollowerKeepPublicationError(t *testing.T) {
	p := newQueuedPublisher(t, 1, 2)
	p.failAt, p.afterRename = 2, true
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b, c := p.start(ctx, queueEnrollment(1)), p.start(ctx, queueEnrollment(2))
	waitPending(t, p.dir, 2)
	p.gates[1].open()
	waitSignal(t, p.gates[2].entered)
	cancel()
	waiting, withdraw := context.WithCancel(t.Context())
	d := p.start(waiting, queueEnrollment(3))
	waitPending(t, p.dir, 1)
	withdraw()
	if err := queuedResult(t, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("new pending cancellation = %v", err)
	}
	waitPending(t, p.dir, 0)
	select {
	case err := <-b:
		t.Fatalf("accepted caller returned before publication: %v", err)
	case err := <-c:
		t.Fatalf("accepted caller returned before publication: %v", err)
	default:
	}
	p.gates[2].open()
	if err := queuedResult(t, holder); err != nil {
		t.Fatal(err)
	}
	for _, result := range []<-chan error{b, c} {
		if err := queuedResult(t, result); !errors.Is(err, unix.EIO) {
			t.Fatalf("accepted error = %v", err)
		}
	}
	p.requireCounts(t, 2)
}

func joinTestCommitter(t *testing.T, path string) (*committer, func(), *os.File) {
	t.Helper()
	dir, err := openDirectory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	c, leave, err := joinCommitter(dir)
	if err != nil {
		_ = dir.Close()
		t.Fatal(err)
	}
	return c, leave, dir
}

func TestCommitterDirectoryIdentityAndLifecycle(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config")
	c, leave, dir := joinTestCommitter(t, path)
	defer func() { leave(); _ = dir.Close() }()
	alias, leaveAlias, aliasDir := joinTestCommitter(t, path+"/.")
	if c != alias {
		t.Fatal("same directory alias got a distinct queue")
	}
	leaveAlias()
	_ = aliasDir.Close()
	moved := filepath.Join(root, "renamed")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	renamed, leaveRenamed, renamedDir := joinTestCommitter(t, moved)
	if c != renamed {
		t.Fatal("rename changed directory identity")
	}
	leaveRenamed()
	_ = renamedDir.Close()
	replacement, leaveReplacement, replacementDir := joinTestCommitter(t, path)
	if c == replacement {
		t.Fatal("replacement directory shared the old queue")
	}
	leaveReplacement()
	_ = replacementDir.Close()
	other, leaveOther, otherDir := joinTestCommitter(t, filepath.Join(root, "distinct"))
	if c == other || replacement == other {
		t.Fatal("distinct directories conflated")
	}
	leaveOther()
	_ = otherDir.Close()
	committersMu.Lock()
	defer committersMu.Unlock()
	for _, entry := range committers {
		if entry == replacement || entry == other {
			t.Fatal("idle registry entry retained")
		}
	}
}

func TestCommitterLastLeaveRacesNewJoin(t *testing.T) {
	path := t.TempDir()
	for range 32 {
		testCommitterLastLeave(t, path)
	}
	closed, err := openDirectory(path, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if _, _, err := joinCommitter(closed); err == nil {
		t.Fatal("metadata failure used a fallback identity")
	}
}

func testCommitterLastLeave(t *testing.T, path string) {
	t.Helper()
	_, leave, dir := joinTestCommitter(t, path)
	defer func() { _ = dir.Close() }()
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Go(func() { <-start; leave() })
	workers.Go(func() {
		<-start
		checkNewCommitterJoin(t, dir)
	})
	close(start)
	workers.Wait()
	committersMu.Lock()
	defer committersMu.Unlock()
	for _, c := range committers {
		if c.users <= 0 {
			t.Fatal("registry retained final leave")
		}
	}
}

func checkNewCommitterJoin(t *testing.T, dir *os.File) {
	t.Helper()
	c, done, err := joinCommitter(dir)
	if err != nil {
		t.Error(err)
		return
	}
	defer done()
	committersMu.Lock()
	defer committersMu.Unlock()
	for _, current := range committers {
		if current == c && current.users > 0 {
			return
		}
	}
	t.Error("new join orphaned by last leave")
}

func TestAcceptedStorageSurvivesPendingCompaction(t *testing.T) {
	c := &committer{}
	ctx, cancel := context.WithCancel(t.Context())
	expired := c.submit(ctx, queueEnrollment(0))
	cancel()
	first := c.submit(t.Context(), queueEnrollment(1))
	second := c.submit(t.Context(), queueEnrollment(2))
	batch := c.accept(first)
	if len(batch) != 2 || batch[0] != first || batch[1] != second {
		t.Fatal("batch lost arrival order")
	}
	for range 32 {
		m := c.submit(t.Context(), queueEnrollment(3))
		c.mu.Lock()
		backing := c.pending[:cap(c.pending)]
		c.mu.Unlock()
		if !c.withdraw(m) {
			t.Fatal("pending mutation was accepted")
		}
		requireClearedQueueTail(t, c, backing)
	}
	if batch[0] != first || batch[1] != second {
		t.Fatal("pending compaction overwrote detached batch")
	}
	if !c.withdraw(expired) {
		t.Fatal("expired mutation accepted")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil {
		t.Fatal("empty queue retained backing storage")
	}
}

func requireClearedQueueTail(t *testing.T, c *committer, backing []*mutation) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range backing[len(c.pending):] {
		if m != nil {
			t.Fatal("queue retained removed closure")
		}
	}
}

func rejectNestedSnapshotChange(state *book, sentinel error) error {
	for i := range state.Peers {
		if len(state.Peers[i].Hosts) == 0 {
			continue
		}
		state.Peers[i].Hosts[0].Addresses[0] = "100.64.9.9"
		state.Peers[i].Hosts[0].TailscaleName = "corrupted"
	}
	return sentinel
}

func TestBatchRestoresNestedAndPostSortRejectionsExactly(t *testing.T) {
	state := book{Version: 1}
	if err := queueEnrollment(0)(&state); err != nil {
		t.Fatal(err)
	}
	state.Peers[0].Hosts = []protocol.FleetHost{testHost(1)}
	checkpoint, err := encodeBook(state)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("nested rejection")
	operations := []func(*book) error{
		queueEnrollment(2),
		func(state *book) error { return rejectNestedSnapshotChange(state, sentinel) },
		func(state *book) error {
			state.Peers[0], state.Peers[1] = state.Peers[1], state.Peers[0]
			state.Peers[0].Hosts = make([]protocol.FleetHost, maximumEntries)
			for i := range state.Peers[0].Hosts {
				state.Peers[0].Hosts[i] = testHost(1000 + i)
			}
			state.Peers[1].Hosts = []protocol.FleetHost{testHost(2000)}
			return nil
		},
		requireBatchCheckpoint,
	}
	batch := make([]*mutation, len(operations))
	for i, op := range operations {
		batch[i] = &mutation{operation: op}
	}
	outcomes := make([]error, len(batch))
	contents, applied, err := applyBatch(state, checkpoint, batch, outcomes)
	if err != nil || !applied {
		t.Fatalf("batch = %v, %v", applied, err)
	}
	if !errors.Is(outcomes[1], sentinel) || outcomes[2] == nil || !strings.Contains(outcomes[2].Error(), "imported host count") || outcomes[3] != nil {
		t.Fatalf("outcomes = %v", outcomes)
	}
	final, err := decodeBook(contents)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireBatchCheckpoint(&final); err != nil {
		t.Fatal(err)
	}
}

func requireBatchCheckpoint(state *book) error {
	want := book{Version: 1}
	if err := queueEnrollment(0)(&want); err != nil {
		return err
	}
	want.Peers[0].Hosts = []protocol.FleetHost{testHost(1)}
	if err := queueEnrollment(2)(&want); err != nil {
		return err
	}
	if err := validateBook(&want); err != nil {
		return err
	}
	if !reflect.DeepEqual(*state, want) {
		return errors.New("following operation observed rejected nested fields, peer order or lost successful header")
	}
	return nil
}

func TestAllRejectedAndNoopBatches(t *testing.T) {
	for _, mode := range []string{"rejected", "acknowledged", "unacknowledged"} {
		t.Run(mode, func(t *testing.T) { testAllRejectedAndNoop(t, mode) })
	}
}

func testAllRejectedAndNoop(t *testing.T, mode string) {
	t.Helper()
	p := newQueuedPublisher(t, 1)
	if mode == "unacknowledged" {
		p.failAt, p.afterRename = 1, true
	}
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	sentinel := errors.New("rejected batch")
	op := func(*book) error { return nil }
	if mode == "rejected" {
		op = func(state *book) error { state.Version = 99; return sentinel }
	}
	b, c := p.start(t.Context(), op), p.start(t.Context(), op)
	waitPending(t, p.dir, 2)
	p.gates[1].open()
	if err := queuedResult(t, holder); errors.Is(err, unix.EIO) != (mode == "unacknowledged") {
		t.Fatalf("holder = %v", err)
	}
	for _, result := range []<-chan error{b, c} {
		err := queuedResult(t, result)
		if mode == "rejected" && !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		if mode != "rejected" && err != nil {
			t.Fatal(err)
		}
	}
	want := int32(1)
	if mode == "unacknowledged" {
		want = 2
	}
	if p.calls.Load() != want {
		t.Fatalf("publications = %d, want %d", p.calls.Load(), want)
	}
	if mode == "unacknowledged" {
		p.requireCounts(t, 2)
	}
}

func TestLockErrorAndCanceledLeaderLeaveOtherPendingWork(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) { testFailedLeader(t, canceled) })
	}
}

func testFailedLeader(t *testing.T, canceled bool) {
	t.Helper()
	c, leave, dir := joinTestCommitter(t, t.TempDir())
	defer func() { leave(); _ = dir.Close() }()
	lock := newMutationLock(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	leader := c.submit(ctx, queueEnrollment(0))
	follower := c.submit(t.Context(), queueEnrollment(1))
	attemptLock := lock
	if canceled {
		cancel()
	} else {
		attemptLock = openSnapshotLock(t, dir.Name())
		_ = attemptLock.Close()
	}
	done, err := c.tryCommit(dir, attemptLock, leader, publishBook)
	if done || (!canceled && err == nil) {
		t.Fatalf("ineligible leader = %v, %v", done, err)
	}
	if !c.withdraw(leader) || c.isAccepted(follower) {
		t.Fatal("failed leader stole other pending work")
	}
	if done, err := c.tryCommit(dir, lock, follower, publishBook); !done || err != nil {
		t.Fatalf("follower progress = %v, %v", done, err)
	}
	if err := queuedResult(t, follower.outcome); err != nil {
		t.Fatal(err)
	}
	if len(leader.outcome) != 0 {
		t.Fatal("withdrawn leader received a batch result")
	}
	peers, err := Peers(dir.Name())
	if err != nil || len(peers) != 1 || peers[0].Identity != testHost(1).ID {
		t.Fatalf("failed leader persisted: %#v, %v", peers, err)
	}
}

func TestReadFailureSettlesBatchAndFreshSubmissionProgresses(t *testing.T) {
	c, leave, dir := joinTestCommitter(t, t.TempDir())
	defer func() { leave(); _ = dir.Close() }()
	lock := newMutationLock(t, dir)
	if err := os.WriteFile(filepath.Join(dir.Name(), bookName), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, second := c.submit(t.Context(), queueEnrollment(0)), c.submit(t.Context(), queueEnrollment(1))
	if done, err := c.tryCommit(dir, lock, first, publishBook); !done || err != nil {
		t.Fatalf("commit = %v, %v", done, err)
	}
	for _, m := range []*mutation{first, second} {
		if err := queuedResult(t, m.outcome); err == nil {
			t.Fatal("bad book accepted")
		}
		if len(m.outcome) != 0 || m.operation != nil {
			t.Fatal("completed batch retained operation or duplicate result")
		}
	}
	if err := os.Remove(filepath.Join(dir.Name(), bookName)); err != nil {
		t.Fatal(err)
	}
	if err := updateState(t.Context(), dir.Name(), queueEnrollment(2)); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceSnapshotOwnsCopiedInputAfterWithdrawal(t *testing.T) {
	p := newQueuedPublisher(t, 1)
	if err := updateState(t.Context(), p.dir, queueEnrollment(1)); err != nil {
		t.Fatal(err)
	}
	state, err := readState(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	hosts := []protocol.FleetHost{testHost(2)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	p.workers.Go(func() { result <- replaceSnapshot(ctx, p.dir, state.Peers[0], hosts) })
	waitPending(t, p.dir, 1)
	c, leave, dir := joinTestCommitter(t, p.dir)
	defer func() { leave(); _ = dir.Close() }()
	c.mu.Lock()
	pending := c.pending[0]
	c.mu.Unlock()
	probe := state
	if err := pending.operation(&probe); err != nil {
		t.Fatal(err)
	}
	retained := probe.Peers[0].Hosts
	if &retained[0] == &hosts[0] || &retained[0].Addresses[0] == &hosts[0].Addresses[0] {
		t.Fatal("snapshot captured caller backing arrays")
	}
	cancel()
	if err := queuedResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	hosts[0].TailscaleName = "changed after withdrawal"
	hosts[0].Addresses[0] = "100.64.1.9"
	if retained[0].TailscaleName != "" || retained[0].Addresses[0] != "100.64.0.2" {
		t.Fatal("withdrawn snapshot retained caller storage")
	}
	waitPending(t, p.dir, 0)
	p.gates[1].open()
	if err := queuedResult(t, holder); err != nil {
		t.Fatal(err)
	}
	requireHosts(t, p.dir)
}

func TestEightSourceChangedSyncAndIdenticalSync(t *testing.T) {
	owner := newController(t)
	var sources []*controllerFixture
	for i := range 8 {
		source := newController(t)
		trust(t, owner, source)
		trust(t, source, owner)
		source.setLocal(testHost(100 + i))
		sources = append(sources, source)
	}
	ready := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	for _, source := range sources {
		source.service.LocalHosts = func(ctx context.Context) ([]protocol.FleetHost, error) {
			ready <- struct{}{}
			return sourceSnapshotAfter(ctx, source, release)
		}
	}
	lock := openSnapshotLock(t, owner.dir)
	if err := acquireLockFor(t.Context(), lock, lockLimit); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	var workers sync.WaitGroup
	t.Cleanup(func() { unblock(); _ = lock.Close(); cancel(); workers.Wait() })
	workers.Go(func() { result <- Sync(ctx, owner.dir, owner.state) })
	for range 8 {
		waitSignal(t, ready)
	}
	unblock()
	waitPending(t, owner.dir, 8)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := queuedResult(t, result); err != nil {
		t.Fatal(err)
	}
	hosts, err := Hosts(owner.dir)
	if err != nil || len(hosts) != 8 {
		t.Fatalf("changed Sync hosts = %d, %v", len(hosts), err)
	}
	before, err := os.Stat(filepath.Join(owner.dir, bookName))
	if err != nil {
		t.Fatal(err)
	}
	if err := Sync(t.Context(), owner.dir, owner.state); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(owner.dir, bookName))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("identical Sync replaced book: %v", err)
	}
}

func sourceSnapshotAfter(ctx context.Context, source *controllerFixture, release <-chan struct{}) ([]protocol.FleetHost, error) {
	select {
	case <-release:
		return source.localHosts(ctx)
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for fixture snapshot release: %w", ctx.Err())
	}
}

func TestEightSourceSyncCommitsAcceptedBatchWithEveryBarrier(t *testing.T) {
	owner := newController(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	expected := make(map[string]protocol.FleetHost)
	for i := range 8 {
		source := newController(t)
		trust(t, owner, source)
		trust(t, source, owner)
		host := testHost(100 + i)
		source.setLocal(host)
		expected[source.host.ID] = host
		if i != 0 {
			holdSourceSnapshots(source, release)
		}
	}
	before, err := readState(owner.dir)
	if err != nil {
		t.Fatal(err)
	}
	observed := newQueuedPublisher(t, 1, 2)
	observed.dir = owner.dir
	previous := syncPublicationFile
	syncPublicationFile = observed.observeProductionSync
	t.Cleanup(func() { syncPublicationFile = previous })
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() { result <- Sync(ctx, owner.dir, owner.state) })
	t.Cleanup(func() { unblock(); openPublicationGates(observed); cancel(); workers.Wait() })
	waitSignal(t, observed.gates[1].entered)
	unblock()
	waitPending(t, owner.dir, 7)
	observed.gates[1].open()
	waitSignal(t, observed.gates[2].entered)
	cancel()
	observed.gates[2].open()
	if err := queuedResult(t, result); err != nil {
		t.Fatalf("accepted Sync did not join with its actual result: %v", err)
	}
	observed.requireCounts(t, 2)
	requireCurrentSourceSnapshots(t, owner.dir, before, expected)
	if err := Sync(t.Context(), owner.dir, owner.state); err != nil {
		t.Fatal(err)
	}
	observed.requireCounts(t, 2)
}

func openPublicationGates(p *queuedPublisher) {
	for _, gate := range p.gates {
		gate.open()
	}
}

func (p *queuedPublisher) observeProductionSync(file *os.File) error {
	switch file.Name() {
	case ".connection-sharing.lock":
		call := p.calls.Add(1)
		if gate := p.gates[call]; gate != nil {
			close(gate.entered)
			<-gate.release
		}
		p.locks.Add(1)
	case p.dir:
		p.dirs.Add(1)
	default:
		p.files.Add(1)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync observed production publication: %w", err)
	}
	return nil
}

func requireCurrentSourceSnapshots(t *testing.T, dir string, before book, expected map[string]protocol.FleetHost) {
	t.Helper()
	after, err := readState(dir)
	if err != nil || len(after.Peers) != len(before.Peers) {
		t.Fatalf("current source state = %#v, %v", after, err)
	}
	for i, peer := range after.Peers {
		if peer.Identity != before.Peers[i].Identity || peer.Generation != before.Peers[i].Generation || !reflect.DeepEqual(peer.Hosts, []protocol.FleetHost{expected[peer.Identity]}) {
			t.Fatalf("source snapshot or generation changed: %#v", peer)
		}
	}
	hosts, err := Hosts(dir)
	if err != nil || len(hosts) != len(expected) {
		t.Fatalf("fresh merged hosts = %#v, %v", hosts, err)
	}
}

func holdSourceSnapshots(source *controllerFixture, release <-chan struct{}) {
	source.service.LocalHosts = func(ctx context.Context) ([]protocol.FleetHost, error) {
		return sourceSnapshotAfter(ctx, source, release)
	}
}

func newMutationLock(t *testing.T, dir *os.File) *os.File {
	t.Helper()
	lock, err := openFile(dir, ".connection-sharing.lock", unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}

func TestEncodingFailureRejectsCandidateAndSettlesAcceptedBatch(t *testing.T) {
	oversized := book{Version: 1}
	hosts := make([]protocol.FleetHost, maximumEntries)
	for i := range hosts {
		hosts[i] = testHost(i)
		hosts[i].Endpoint = "ws://x/" + strings.Repeat("x", 300)
	}
	for i := range maximumEntries {
		host := testHost(1000 + i)
		oversized.Peers = append(oversized.Peers, enrollment{Peer: Peer{Identity: host.ID, Endpoint: host.Endpoint}, Generation: strings.Repeat("0", 32), Hosts: hosts})
	}
	compact, err := json.Marshal(oversized)
	if err != nil || len(compact) >= maximumBytes {
		t.Fatalf("compact fixture size = %d, %v", len(compact), err)
	}
	before := book{Version: 1}
	checkpoint, err := encodeBook(before)
	if err != nil {
		t.Fatal(err)
	}
	batch := []*mutation{
		{operation: func(state *book) error { *state = oversized; return nil }},
		{operation: func(state *book) error {
			if !reflect.DeepEqual(*state, before) {
				return errors.New("encoding rejection changed next operation")
			}
			return nil
		}},
	}
	outcomes := make([]error, len(batch))
	contents, applied, err := applyBatch(before, checkpoint, batch, outcomes)
	if err != nil || !applied || string(contents) != string(checkpoint) || outcomes[0] == nil || outcomes[1] != nil {
		t.Fatalf("encoding rollback: %v, %v, %v", applied, err, outcomes)
	}
	c, leave, dir := joinTestCommitter(t, t.TempDir())
	defer func() { leave(); _ = dir.Close() }()
	lock := newMutationLock(t, dir)
	if err := os.WriteFile(filepath.Join(dir.Name(), bookName), compact, 0o600); err != nil {
		t.Fatal(err)
	}
	var executed, published atomic.Int32
	op := func(*book) error { executed.Add(1); return nil }
	first, second := c.submit(t.Context(), op), c.submit(t.Context(), op)
	publish := func(*os.File, *os.File, []byte) error { published.Add(1); return nil }
	if done, err := c.tryCommit(dir, lock, first, publish); !done || err != nil {
		t.Fatalf("initial encoding commit = %v, %v", done, err)
	}
	for _, m := range []*mutation{first, second} {
		if err := queuedResult(t, m.outcome); err == nil || !strings.Contains(err.Error(), "configuration exceeds 32 MiB") {
			t.Fatalf("initial encoding error = %v", err)
		}
	}
	if executed.Load() != 0 || published.Load() != 0 {
		t.Fatal("initial encoding failure executed or published a mutation")
	}
	if err := os.Remove(filepath.Join(dir.Name(), bookName)); err != nil {
		t.Fatal(err)
	}
	if err := updateState(t.Context(), dir.Name(), queueEnrollment(0)); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreFailureKeepsOwnRejection(t *testing.T) {
	sentinel := errors.New("own rejection before restore")
	batch := []*mutation{{operation: func(*book) error { return sentinel }}, {operation: queueEnrollment(1)}}
	outcomes := make([]error, len(batch))
	_, applied, err := applyBatch(book{Version: 1}, []byte("invalid checkpoint"), batch, outcomes)
	if applied || err == nil || !strings.Contains(err.Error(), "fleet: restore configuration after rejected change") || !errors.Is(outcomes[0], sentinel) {
		t.Fatalf("restore error = %v, outcomes = %v", err, outcomes)
	}
}

func TestRenamedDirectoryAndReplacementMutateIndependently(t *testing.T) {
	p := newQueuedPublisher(t, 1)
	holder := p.start(t.Context(), queueEnrollment(0))
	waitSignal(t, p.gates[1].entered)
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(p.dir, moved); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	p.workers.Go(func() { result <- updateStateWithPublisher(t.Context(), moved, queueEnrollment(1), p.publish) })
	waitPending(t, moved, 1)
	if err := updateState(t.Context(), p.dir, queueEnrollment(2)); err != nil {
		t.Fatal(err)
	}
	p.gates[1].open()
	for _, ch := range []<-chan error{holder, result} {
		if err := queuedResult(t, ch); err != nil {
			t.Fatal(err)
		}
	}
	original, err := Peers(moved)
	if err != nil || len(original) != 2 {
		t.Fatalf("renamed peers = %d, %v", len(original), err)
	}
	replacement, err := Peers(p.dir)
	if err != nil || len(replacement) != 1 || replacement[0].Identity != testHost(2).ID {
		t.Fatalf("replacement peers = %v, %v", replacement, err)
	}
	p.requireCounts(t, 2)
}
