package tunnel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

type testActivation struct {
	endpoint Endpoint
}

type testActivator struct {
	mu     sync.Mutex
	claims map[string]bool
	active map[string]*testActivation
}

func (a *testActivator) ActivateTunnel(_ context.Context, _, hostname string, endpoint Endpoint) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.claims[hostname] {
		return nil, ErrNotFound
	}
	if a.active[hostname] != nil {
		return nil, ErrActive
	}
	activation := &testActivation{endpoint}
	a.active[hostname] = activation
	return func() {
		a.mu.Lock()
		if a.active[hostname] == activation {
			delete(a.active, hostname)
		}
		a.mu.Unlock()
		_ = endpoint.Close()
	}, nil
}

func (a *testActivator) endpoint(hostname string) Endpoint {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[hostname] == nil {
		return nil
	}
	return a.active[hostname].endpoint
}

type sshFixture struct {
	address   string
	key       ed25519.PrivateKey
	activator *testActivator
}

func newSSHFixture(t *testing.T, interval time.Duration, names ...string) sshFixture {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	activator := &testActivator{claims: make(map[string]bool), active: make(map[string]*testActivation)}
	for _, name := range names {
		activator.claims[name] = true
	}
	server := &charmssh.Server{PublicKeyHandler: func(_ charmssh.Context, _ charmssh.PublicKey) bool { return true }}
	server.AddHostKey(signer)
	if err := server.SetOption(sshOption(activator, interval)); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()
	return sshFixture{listener.Addr().String(), key, activator}
}

func (f sshFixture) client(t *testing.T, answerKeepalive bool) (gossh.Conn, <-chan gossh.NewChannel) {
	return f.clientWithConn(t, answerKeepalive, func(conn net.Conn) net.Conn { return conn })
}

func (f sshFixture) clientWithConn(t *testing.T, answerKeepalive bool, wrap func(net.Conn) net.Conn) (gossh.Conn, <-chan gossh.NewChannel) {
	t.Helper()
	signer, err := gossh.NewSignerFromKey(f.key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Dial("tcp", f.address)
	if err != nil {
		t.Fatal(err)
	}
	raw = wrap(raw)
	config := &gossh.ClientConfig{User: "mesh", Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)}, HostKeyCallback: gossh.InsecureIgnoreHostKey()} //nolint:gosec // loopback test server
	conn, channels, requests, err := gossh.NewClientConn(raw, f.address, config)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go answerSSHRequests(requests, answerKeepalive)
	return conn, channels
}

type sshReadGate struct {
	net.Conn
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

func (c *sshReadGate) Read(buffer []byte) (int, error) {
	n, err := c.Conn.Read(buffer)
	c.mu.Lock()
	gate := c.gate
	c.mu.Unlock()
	if gate != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-gate
	}
	return n, err
}

func (c *sshReadGate) block() {
	c.mu.Lock()
	c.gate = make(chan struct{})
	c.mu.Unlock()
}

func (c *sshReadGate) unblock() {
	c.mu.Lock()
	if c.gate != nil {
		close(c.gate)
		c.gate = nil
	}
	c.mu.Unlock()
}

func TestSSHChannelBudgetWaitsForPeerClose(t *testing.T) {
	const name = "blog.shaulavo.dev"
	f := newSSHFixture(t, time.Second, name)
	var gate *sshReadGate
	conn, channels := f.clientWithConn(t, true, func(raw net.Conn) net.Conn {
		gate = &sshReadGate{Conn: raw, entered: make(chan struct{}, 1)}
		return gate
	})
	t.Cleanup(gate.unblock)
	if !sendForward(t, conn, "tcpip-forward", name, 80) {
		t.Fatal("forward refused")
	}
	endpoint := f.activator.endpoint(name)
	accepted := make(chan gossh.Channel, maximumStreamsPerForward)
	go acceptForwardChannels(channels, accepted)
	for range maximumStreamsPerForward {
		stream, err := endpoint.Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stream.Close() })
	}
	gate.block()
	for range maximumStreamsPerForward {
		if err := (<-accepted).CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("peer did not receive the server's channel close")
	}
	// EOF finishes the copying goroutines. The peer's close acknowledgement
	// remains blocked, so every SSH channel still owns its budget.
	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := endpoint.Dial(ctx); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unretired channel budget was recycled: %v", err)
	}
	gate.unblock()
	concrete := endpoint.(*sshEndpoint)
	waitTunnel(t, func() bool { return len(concrete.slots) == 0 })
	stream, err := endpoint.Dial(context.Background())
	if err != nil {
		t.Fatalf("acknowledged channel budget not reusable: %v", err)
	}
	_ = stream.Close()
}

func answerSSHRequests(requests <-chan *gossh.Request, answer bool) {
	for request := range requests {
		if answer {
			_ = request.Reply(false, nil)
		}
	}
}

func sendForward(t *testing.T, conn gossh.Conn, kind, hostname string, port uint32) bool {
	t.Helper()
	ok, _, err := conn.SendRequest(kind, true, gossh.Marshal(&forwardRequest{hostname, port}))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestSSHForwardValidationAndCancellation(t *testing.T) {
	const name = "blog.shaulavo.dev"
	f := newSSHFixture(t, time.Second, name)
	conn, _ := f.client(t, true)
	for _, bad := range []string{"", "blog", "*", "*.shaulavo.dev", "localhost", "127.0.0.1", "::1", "other.shaulavo.dev", "blog.other.dev", "BLOG.shaulavo.dev"} {
		if sendForward(t, conn, "tcpip-forward", bad, 80) {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, port := range []uint32{0, 22, 443, 65535} {
		if sendForward(t, conn, "tcpip-forward", name, port) {
			t.Fatalf("accepted port %d", port)
		}
	}
	if !sendForward(t, conn, "tcpip-forward", name, 80) {
		t.Fatal("forward refused")
	}
	if f.activator.endpoint(name) == nil {
		t.Fatal("acknowledged before activation")
	}
	if sendForward(t, conn, "tcpip-forward", name, 80) {
		t.Fatal("accepted duplicate")
	}
	if _, _, err := conn.OpenChannel("direct-tcpip", nil); err == nil {
		t.Fatal("accepted local forward")
	}
	if !sendForward(t, conn, "cancel-tcpip-forward", name, 80) {
		t.Fatal("cancel refused")
	}
	if f.activator.endpoint(name) != nil {
		t.Fatal("cancel acknowledged before deactivation")
	}
	if !sendForward(t, conn, "tcpip-forward", name, 80) {
		t.Fatal("reconnect refused")
	}
	_ = conn.Close()
	waitTunnel(t, func() bool { return f.activator.endpoint(name) == nil })
}

func TestSSHForwardLimitAcrossConnections(t *testing.T) {
	names := make([]string, MaximumForwardsPerKey+1)
	for index := range names {
		names[index] = fmt.Sprintf("f%d.shaulavo.dev", index)
	}
	f := newSSHFixture(t, time.Second, names...)
	first, _ := f.client(t, true)
	second, _ := f.client(t, true)
	for index, name := range names[:MaximumForwardsPerKey] {
		connection := first
		if index%2 != 0 {
			connection = second
		}
		if !sendForward(t, connection, "tcpip-forward", name, 80) {
			t.Fatalf("forward %d refused", index)
		}
	}
	if sendForward(t, second, "tcpip-forward", names[MaximumForwardsPerKey], 80) {
		t.Fatal("per-key limit bypassed")
	}
	if !sendForward(t, first, "cancel-tcpip-forward", names[0], 80) {
		t.Fatal("cancel refused")
	}
	if !sendForward(t, second, "tcpip-forward", names[MaximumForwardsPerKey], 80) {
		t.Fatal("released slot not reusable")
	}
}

func TestSSHKeepaliveExpiresWithoutReply(t *testing.T) {
	const name = "blog.shaulavo.dev"
	f := newSSHFixture(t, 20*time.Millisecond, name)
	conn, _ := f.client(t, false)
	started := time.Now()
	if !sendForward(t, conn, "tcpip-forward", name, 80) {
		t.Fatal("forward refused")
	}
	waitTunnel(t, func() bool { return f.activator.endpoint(name) == nil })
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("expiry took %s", elapsed)
	}
}

func TestSSHKeepaliveRepliesPreserveForward(t *testing.T) {
	const name = "blog.shaulavo.dev"
	f := newSSHFixture(t, 10*time.Millisecond, name)
	conn, _ := f.client(t, true)
	if !sendForward(t, conn, "tcpip-forward", name, 80) {
		t.Fatal("forward refused")
	}
	time.Sleep(80 * time.Millisecond)
	if f.activator.endpoint(name) == nil {
		t.Fatal("replying client expired")
	}
}

func TestSSHDisconnectStartsEveryReleaseBeforeWaiting(t *testing.T) {
	names := []string{"first.shaulavo.dev", "second.shaulavo.dev"}
	f := newSSHFixture(t, time.Second, names...)
	conn, _ := f.client(t, true)
	for _, name := range names {
		if !sendForward(t, conn, "tcpip-forward", name, 80) {
			t.Fatal("forward refused")
		}
	}
	state := f.activator.endpoint(names[0]).(*sshEndpoint).connection
	started := make(chan struct{}, len(names))
	finish := make(chan struct{})
	defer close(finish)
	state.mu.Lock()
	for name, release := range state.forwards {
		state.forwards[name] = func() { started <- struct{}{}; <-finish; release() }
	}
	state.mu.Unlock()
	_ = conn.Close()
	for range names {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("one blocked release delayed another hostname's cleanup")
		}
	}
}

func TestSSHDialCancellationUnpublishesStalledChannelOpen(t *testing.T) {
	const name = "blog.shaulavo.dev"
	f := newSSHFixture(t, time.Second, name)
	conn, channels := f.client(t, true)
	if !sendForward(t, conn, "tcpip-forward", name, 80) {
		t.Fatal("forward refused")
	}
	go func() { <-channels }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := f.activator.endpoint(name).Dial(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial error = %v", err)
	}
	if f.activator.endpoint(name) != nil {
		t.Fatal("stalled open returned before deactivation")
	}
}

func TestSSHChannelBudgetDeadlinesAndBackpressure(t *testing.T) {
	const name = "blog.shaulavo.dev"
	f := newSSHFixture(t, time.Second, name)
	conn, channels := f.client(t, true)
	if !sendForward(t, conn, "tcpip-forward", name, 80) {
		t.Fatal("forward refused")
	}
	endpoint := f.activator.endpoint(name)
	accepted := make(chan gossh.Channel, maximumStreamsPerForward)
	go acceptForwardChannels(channels, accepted)
	var streams []net.Conn
	for range maximumStreamsPerForward {
		stream, err := endpoint.Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, stream)
		t.Cleanup(func() { _ = stream.Close() })
	}
	if _, err := endpoint.Dial(context.Background()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("overflow Dial = %v", err)
	}
	stream := streams[0]
	if err := stream.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Read(make([]byte, 1)); !os.IsTimeout(err) {
		t.Fatalf("deadline error = %v", err)
	}
	if err := stream.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	remote := <-accepted
	written := make(chan error, 1)
	go func() { _, err := remote.Write(make([]byte, MaximumBytesPerForward*2)); written <- err }()
	select {
	case err := <-written:
		t.Fatalf("unread channel did not apply backpressure: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	_ = conn.Close()
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("connection close did not unblock writer")
	}
}

func acceptForwardChannels(channels <-chan gossh.NewChannel, accepted chan<- gossh.Channel) {
	for request := range channels {
		channel, requests, err := request.Accept()
		if err != nil {
			return
		}
		go gossh.DiscardRequests(requests)
		accepted <- channel
	}
}

func TestStockSSHNoSessionReverseForward(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("stock ssh unavailable")
	}
	const name = "blog.shaulavo.dev"
	f := newSSHFixture(t, time.Second, name)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "from laptop") }))
	t.Cleanup(origin.Close)
	block, err := gossh.MarshalPrivateKey(f.key, "test")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "identity.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(f.address)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, ssh, "-N", "-F", "/dev/null", "-o", "ExitOnForwardFailure=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-i", keyPath, "-p", port, "-R", name+":80:"+strings.TrimPrefix(origin.URL, "http://"), host) //nolint:gosec // stock SSH integration against generated loopback test endpoints
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = command.Wait() })
	waitTunnel(t, func() bool { return f.activator.endpoint(name) != nil })
	stream, err := f.activator.endpoint(name).Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := fmt.Fprintf(stream, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", name); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "from laptop" {
		t.Fatalf("response = %q, %v", body, err)
	}
	cancel()
	waitTunnel(t, func() bool { return f.activator.endpoint(name) == nil })
}

func waitTunnel(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("tunnel state did not converge")
}
