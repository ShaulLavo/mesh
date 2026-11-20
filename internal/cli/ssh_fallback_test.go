package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func newBoundaryPeer(capacity int) *setupPeer {
	return &setupPeer{openGate: make(chan struct{}), execGate: make(chan struct{}), holdGate: make(chan struct{}),
		opens: make(chan struct{}, capacity), execs: make(chan string, capacity), cleanupSeen: make(chan struct{}, 1)}
}

func boundaryState(t *testing.T) string {
	t.Helper()
	state := compactSocketTempDir(t)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	return state
}

func newBoundaryPool(t *testing.T) *sshCommandPool {
	t.Helper()
	return &sshCommandPool{ctx: t.Context(), entries: make(map[sshPoolKey]*sshPoolEntry), dial: dialMeshSSHWithKey}
}

func cleanupBoundaryPool(t *testing.T, pool *sshCommandPool, stopOwner func(), commands *sync.WaitGroup) {
	t.Helper()
	t.Cleanup(func() {
		stopOwner()
		pool.connecting.Wait()
		pool.expire(time.Now(), true)
		commands.Wait()
		pool.setups.Wait()
		pool.lifetimes.Wait()
	})
}

func replaceSSHOwnerStart(t *testing.T, start func(context.Context) error) {
	t.Helper()
	previous := startSSHOwnerProcess
	startSSHOwnerProcess = start
	t.Cleanup(func() { startSSHOwnerProcess = previous })
}

func poolLive(pool *sshCommandPool) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.live
}

func TestSSHPipeCommandsBeyondPoolLimitsRunDirect(t *testing.T) {
	// One destination past its 16 commands, and three destinations past the
	// owner's 32 command slots: the extra command runs on its own connection.
	t.Run("per-host", func(t *testing.T) { assertCommandsBeyondPool(t, 1, sshPoolChannels+1, sshPoolChannels) })
	t.Run("owner-slots", func(t *testing.T) { assertCommandsBeyondPool(t, 3, 11, 32) })
}

func assertCommandsBeyondPool(t *testing.T, servers, perServer, pooled int) {
	total := servers * perServer
	peer := newBoundaryPeer(total + 1)
	hosts := make([]HostRecord, 0, servers)
	ports := make([]uint16, 0, servers)
	for range servers {
		host, port := poolTestServerWith(t, peer.channel)
		hosts, ports = append(hosts, host), append(ports, port)
	}
	state := boundaryState(t)
	writeBoundaryHosts(t, state, hosts...)
	pool := newBoundaryPool(t)
	var commands sync.WaitGroup
	cleanupBoundaryPool(t, pool, boundaryOwnerListener(t, pool, state), &commands)
	results := make([]<-chan boundaryResult, 0, total)
	for index, host := range hosts {
		for range perServer {
			results = append(results, startBoundaryCommand(t.Context(), t, host, ports[index], "hold", &commands))
		}
	}
	for range total {
		awaitBoundaryStage(t, peer, false, "hold")
	}
	awaitSSHPoolUsers(t, pool, pooled)
	close(peer.holdGate)
	for _, result := range results {
		assertBoundarySuccess(t, awaitBoundaryResult(t, result))
	}
}

func TestSSHPoolEvictsLeastRecentlyUsedIdleDestination(t *testing.T) {
	host, port := poolTestServer(t)
	state := t.TempDir()
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	pool := newBoundaryPool(t)
	t.Cleanup(func() { pool.connecting.Wait(); pool.expire(time.Now(), true); pool.lifetimes.Wait() })
	// Distinct endpoints give distinct destinations on one fixture listener.
	destination := func(index int) HostRecord {
		value := host
		value.Endpoint = fmt.Sprintf("ws://127.0.0.1:%d/mesh", index+1)
		return value
	}
	key := func(index int) sshPoolKey {
		value, _, err := sshCommandKey(destination(index), state, port)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := time.Now()
	for index := range sshPoolHosts {
		_, release, err := pool.acquire(t.Context(), destination(index), state, port)
		if err != nil {
			t.Fatal(err)
		}
		release()
		pool.mu.Lock()
		pool.entries[key(index)].idle = base.Add(time.Duration(index) * time.Second)
		pool.mu.Unlock()
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	_, release, err := pool.acquire(t.Context(), destination(sshPoolHosts), state, port)
	if err != nil {
		t.Fatalf("ninth destination with idle transports: %v", err)
	}
	releases = append(releases, release)
	pool.mu.Lock()
	_, oldest := pool.entries[key(0)]
	_, newer := pool.entries[key(1)]
	live := pool.live
	pool.mu.Unlock()
	if oldest || !newer || live != sshPoolHosts {
		t.Fatalf("eviction kept oldest=%v newer=%v live=%d", oldest, newer, live)
	}
	for index := 1; index < sshPoolHosts; index++ {
		_, release, err := pool.acquire(t.Context(), destination(index), state, port)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, _, err := pool.acquire(t.Context(), destination(sshPoolHosts+1), state, port); !errors.Is(err, errSSHPoolFull) {
		t.Fatalf("busy destinations: %v, want errSSHPoolFull", err)
	}
	for _, release := range releases {
		release()
	}
	releases = nil
	pool.expire(time.Now().Add(2*sshPoolIdle), false)
	pool.lifetimes.Wait()
	if live := poolLive(pool); live != 0 {
		t.Fatalf("live transport accounting after eviction=%d", live)
	}
}

func TestSSHOwnerStartWaitsForExitingOwner(t *testing.T) {
	peer := newBoundaryPeer(2)
	host, port := poolTestServerWith(t, peer.channel)
	state := boundaryState(t)
	writeBoundaryHosts(t, state, host)
	// The previous owner has closed its socket but still holds its lock while
	// it finishes shutting down.
	previous, err := openSSHOwnerLock(state, "ssh-owner.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(previous.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var starts atomic.Int32
	var waited atomic.Bool
	var served error
	replaceSSHOwnerStart(t, func(context.Context) error {
		starts.Add(1)
		ready := make(chan struct{})
		go func() {
			defer close(done)
			served = ownSSHHelper(ctx, state, func() { close(ready) }, func() { waited.Store(true); _ = previous.Close() })
		}()
		select {
		case <-ready:
			return nil
		case <-done:
			return served
		}
	})
	t.Cleanup(func() {
		cancel()
		if starts.Load() != 0 {
			<-done
		}
	})
	var commands sync.WaitGroup
	value := awaitBoundaryResult(t, startBoundaryCommand(t.Context(), t, host, port, "sibling", &commands))
	commands.Wait()
	assertBoundarySuccess(t, value)
	if !waited.Load() || starts.Load() != 1 {
		t.Fatalf("new owner waited=%v starts=%d", waited.Load(), starts.Load())
	}
	select {
	case <-done:
		t.Fatalf("new owner exited instead of serving: %v", served)
	default:
	}
}

func TestSSHOwnerEndingBeforeAcceptanceRetriesOnce(t *testing.T) {
	// One lost owner is retried through the shared owner; a second loss runs
	// the command on its own connection. Each loss happens before acceptance.
	t.Run("retry", func(t *testing.T) { assertOwnerLossBeforeAcceptance(t, 1, 1) })
	t.Run("direct", func(t *testing.T) { assertOwnerLossBeforeAcceptance(t, 2, 0) })
}

func assertOwnerLossBeforeAcceptance(t *testing.T, losses int32, pooled int) {
	peer := newBoundaryPeer(2)
	host, port := poolTestServerWith(t, peer.channel)
	state := boundaryState(t)
	writeBoundaryHosts(t, state, host)
	replaceSSHOwnerStart(t, func(context.Context) error { return errors.New("fixture owner is already listening") })
	pool := newBoundaryPool(t)
	var connections atomic.Int32
	stop := fixtureSSHOwner(t, state, func(conn *net.UnixConn) bool {
		if connections.Add(1) <= losses {
			_ = conn.Close()
			return true
		}
		return false
	}, pool)
	var commands sync.WaitGroup
	cleanupBoundaryPool(t, pool, stop, &commands)
	assertBoundarySuccess(t, awaitBoundaryResult(t, startBoundaryCommand(t.Context(), t, host, port, "sibling", &commands)))
	if connections.Load() != 2 {
		t.Fatalf("owner connections=%d, want one retry", connections.Load())
	}
	if live := poolLive(pool); live != pooled {
		t.Fatalf("shared transports=%d, want %d", live, pooled)
	}
}

func TestSSHOwnerEndingAfterAcceptanceIsNotRetried(t *testing.T) {
	peer := newBoundaryPeer(2)
	host, port := poolTestServerWith(t, peer.channel)
	state := boundaryState(t)
	writeBoundaryHosts(t, state, host)
	replaceSSHOwnerStart(t, func(context.Context) error { return errors.New("fixture owner is already listening") })
	var connections atomic.Int32
	stop := fixtureSSHOwner(t, state, func(conn *net.UnixConn) bool {
		connections.Add(1)
		files, err := receiveSSHFiles(conn)
		closeSSHFiles(files)
		var request sshHelperRequest
		if err == nil && json.NewDecoder(conn).Decode(&request) == nil {
			_, _ = conn.Write([]byte{sshOwnerAccepted})
		}
		_ = conn.Close()
		return true
	}, nil)
	t.Cleanup(stop)
	var commands sync.WaitGroup
	value := awaitBoundaryResult(t, startBoundaryCommand(t.Context(), t, host, port, "sibling", &commands))
	commands.Wait()
	if value.result.Error == "" {
		t.Fatalf("accepted command lost by its owner succeeded: %+v", value)
	}
	if connections.Load() != 1 || len(peer.execs) != 0 {
		t.Fatalf("accepted command was retried: owner connections=%d remote runs=%d", connections.Load(), len(peer.execs))
	}
}

func TestSSHUnreachableOwnerRunsDirect(t *testing.T) {
	peer := newBoundaryPeer(2)
	host, port := poolTestServerWith(t, peer.channel)
	state := boundaryState(t)
	writeBoundaryHosts(t, state, host)
	var starts atomic.Int32
	replaceSSHOwnerStart(t, func(context.Context) error {
		starts.Add(1)
		return errors.New("fixture owner cannot start")
	})
	var commands sync.WaitGroup
	value := awaitBoundaryResult(t, startBoundaryCommand(t.Context(), t, host, port, "sibling", &commands))
	commands.Wait()
	assertBoundarySuccess(t, value)
	if starts.Load() != 1 {
		t.Fatalf("owner starts=%d", starts.Load())
	}
}

func TestSSHPoolReopensChannelOnLostTransport(t *testing.T) {
	peer := newBoundaryPeer(2)
	host, port := poolTestServerWith(t, peer.channel)
	state := boundaryState(t)
	writeBoundaryHosts(t, state, host)
	key, private, err := sshCommandKey(host, state, port)
	if err != nil {
		t.Fatal(err)
	}
	// A transport that died before anything noticed: no disconnect has been
	// observed, so the pool still offers it.
	stale, err := dialMeshSSHWithKey(t.Context(), host, private, port)
	if err != nil {
		t.Fatal(err)
	}
	_ = stale.Close()
	ready := make(chan struct{})
	close(ready)
	entry := &sshPoolEntry{ready: ready, client: stale, idle: time.Now()}
	var dials atomic.Int32
	pool := newBoundaryPool(t)
	pool.dial = func(ctx context.Context, host HostRecord, key ed25519.PrivateKey, port uint16) (*ssh.Client, error) {
		dials.Add(1)
		return dialMeshSSHWithKey(ctx, host, key, port)
	}
	pool.entries[key], pool.live = entry, 1
	var commands sync.WaitGroup
	cleanupBoundaryPool(t, pool, boundaryOwnerListener(t, pool, state), &commands)
	assertBoundarySuccess(t, awaitBoundaryResult(t, startBoundaryCommand(t.Context(), t, host, port, "sibling", &commands)))
	pool.mu.Lock()
	current, dead := pool.entries[key], entry.dead
	pool.mu.Unlock()
	if dials.Load() != 1 || current == entry || !dead {
		t.Fatalf("lost transport retry: dials=%d replaced=%v dead=%v", dials.Load(), current != entry, dead)
	}
}

// fixtureSSHOwner listens on the owner socket. handle returns true when it
// consumed a connection; otherwise a real owner admits it into pool.
func fixtureSSHOwner(t *testing.T, state string, handle func(*net.UnixConn) bool, pool *sshCommandPool) func() {
	t.Helper()
	path := filepath.Join(state, "ssh-command.sock")
	_ = os.Remove(path)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	slots := make(chan struct{}, 32)
	var handlers sync.WaitGroup
	var last atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			if !handle(conn) {
				admitSSHHelper(ctx, conn, state, pool, slots, &handlers, &last)
			}
		}
	}()
	return func() { cancel(); _ = listener.Close(); <-done; handlers.Wait() }
}
