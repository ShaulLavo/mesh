package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"golang.org/x/crypto/ssh"
)

func poolTestServer(t *testing.T) (HostRecord, uint16) {
	t.Helper()
	owner, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var connections sync.WaitGroup
	done := make(chan struct{})
	go acceptPoolTestConnections(ctx, listener, config, &connections, done)
	t.Cleanup(func() { cancel(); _ = listener.Close(); <-done; connections.Wait() })
	return HostRecord{MeshIdentity: owner.ID, Endpoint: "ws://127.0.0.1:1/mesh"}, uint16(listener.Addr().(*net.TCPAddr).Port) //nolint:gosec // OS-assigned port
}

func acceptPoolTestConnections(ctx context.Context, listener net.Listener, config *ssh.ServerConfig, connections *sync.WaitGroup, done chan<- struct{}) {
	defer close(done)
	for {
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		connections.Go(func() { rejectPoolTestChannels(ctx, raw, config) })
	}
}

func rejectPoolTestChannels(ctx context.Context, raw net.Conn, config *ssh.ServerConfig) {
	defer raw.Close() //nolint:errcheck // fixture owns the stream
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	conn, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck // fixture shutdown
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		_ = channel.Reject(ssh.Prohibited, "test channel denied")
	}
}

func TestSSHPoolFirstWaiterCancellationDoesNotCancelDial(t *testing.T) {
	host, port := poolTestServer(t)
	state := t.TempDir()
	_, snapshot, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	var dials atomic.Int32
	pool := &sshCommandPool{ctx: t.Context(), entries: make(map[sshPoolKey]*sshPoolEntry)}
	pool.dial = func(ctx context.Context, target HostRecord, key ed25519.PrivateKey, port uint16) (*ssh.Client, error) {
		dials.Add(1)
		close(entered)
		<-resume
		if !bytes.Equal(key, snapshot) {
			return nil, errors.New("key snapshot changed")
		}
		return dialMeshSSHWithKey(ctx, target, key, port)
	}
	t.Cleanup(func() { pool.connecting.Wait(); pool.expire(time.Now(), true) })
	first, cancel := context.WithCancel(t.Context())
	failed := make(chan error, 1)
	go func() {
		_, release, err := pool.acquire(first, host, state, port)
		if release != nil {
			release()
		}
		failed <- err
	}()
	<-entered
	second := make(chan *ssh.Client, 1)
	go func() {
		client, release, err := pool.acquire(t.Context(), host, state, port)
		if err == nil {
			release()
		}
		second <- client
	}()
	awaitSSHPoolUsers(t, pool, 2)
	cancel()
	if err := <-failed; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter: %v", err)
	}
	close(resume)
	client := <-second
	if client == nil || dials.Load() != 1 {
		t.Fatalf("client %v, dials %d", client, dials.Load())
	}
	for range 2 {
		if _, err := client.NewSession(); err == nil {
			t.Fatal("channel denial was ignored")
		}
	}
	again, release, err := pool.acquire(t.Context(), host, state, port)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if again != client || dials.Load() != 1 {
		t.Fatal("channel denial discarded a live shared transport")
	}
}

func awaitSSHPoolUsers(t *testing.T, pool *sshCommandPool, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		pool.mu.Lock()
		users := 0
		for _, entry := range pool.entries {
			users += entry.users
		}
		pool.mu.Unlock()
		if users == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pool waiters did not reach the requested state")
}

func TestSSHPoolBoundsAndIdleExpiry(t *testing.T) {
	pool := &sshCommandPool{entries: make(map[sshPoolKey]*sshPoolEntry)}
	key := sshPoolKey{Identity: "host"}
	for range sshPoolChannels {
		if _, _, err := pool.reserve(key); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := pool.reserve(key); err == nil {
		t.Fatal("unbounded per-host channels")
	}
	for index := 1; index < sshPoolHosts; index++ {
		if _, _, err := pool.reserve(sshPoolKey{Identity: fmt.Sprint(index)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := pool.reserve(sshPoolKey{Identity: "excess"}); err == nil {
		t.Fatal("unbounded transports")
	}
	pool.expire(time.Now().Add(2*sshPoolIdle), false)
	if len(pool.entries) != sshPoolHosts {
		t.Fatal("expiry removed an active/pending transport")
	}
	for key, entry := range pool.entries {
		entry.users = 0
		entry.idle = time.Now()
		pool.entries[key] = entry
	}
	pool.expire(time.Now(), false)
	if len(pool.entries) != sshPoolHosts {
		t.Fatal("fresh idle transports expired")
	}
	pool.expire(time.Now().Add(2*sshPoolIdle), false)
	if len(pool.entries) != 0 {
		t.Fatal("idle transports retained")
	}
}

func TestSSHPoolKeyIncludesCurrentIdentityEndpointAndPort(t *testing.T) {
	state := t.TempDir()
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{MeshIdentity: "one", Endpoint: "ws://127.0.0.1:1"}
	first, key, err := sshCommandKey(host, state, 22)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatal("missing immutable key snapshot")
	}
	other := t.TempDir()
	if _, _, err := identity.LoadOrCreate(other); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []struct {
		host  HostRecord
		state string
		port  uint16
	}{
		{HostRecord{MeshIdentity: "two", Endpoint: host.Endpoint}, state, 22},
		{HostRecord{MeshIdentity: host.MeshIdentity, Endpoint: "ws://127.0.0.2:1"}, state, 22},
		{host, state, 23}, {host, other, 22},
	} {
		next, _, err := sshCommandKey(changed.host, changed.state, changed.port)
		if err != nil || next == first {
			t.Fatalf("key invalidation: %+v, %v", next, err)
		}
	}
}

func TestSSHPoolFailedDialHasBoundedCooldownWithoutRetry(t *testing.T) {
	state := t.TempDir()
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	pool := &sshCommandPool{ctx: t.Context(), entries: make(map[sshPoolKey]*sshPoolEntry),
		dial: func(context.Context, HostRecord, ed25519.PrivateKey, uint16) (*ssh.Client, error) {
			dials.Add(1)
			return nil, errors.New("fixture unavailable")
		}}
	host := HostRecord{MeshIdentity: "fixture", Endpoint: "ws://127.0.0.1:1"}
	for range 3 {
		if _, _, err := pool.acquire(t.Context(), host, state, 22); err == nil {
			t.Fatal("failed dial succeeded")
		}
	}
	if dials.Load() != 1 {
		t.Fatalf("reconnected inside cooldown: %d", dials.Load())
	}
	pool.mu.Lock()
	for _, entry := range pool.entries {
		entry.retry = time.Now().Add(-time.Second)
	}
	pool.mu.Unlock()
	if _, _, err := pool.acquire(t.Context(), host, state, 22); err == nil {
		t.Fatal("failed dial succeeded")
	}
	pool.connecting.Wait()
	if dials.Load() != 2 {
		t.Fatalf("future call did not reconnect: %d", dials.Load())
	}
}
