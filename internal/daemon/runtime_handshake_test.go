package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/transport"
)

func TestDaemonShutdownDrainsPendingAuthentication(t *testing.T) {
	t.Run("approved", func(t *testing.T) { testShutdownHandshake(t, true, true) })
	t.Run("denied", func(t *testing.T) { testShutdownHandshake(t, false, true) })
	t.Run("revoked before acceptance", func(t *testing.T) { testShutdownHandshake(t, true, false) })
}

func TestDaemonShutdownKeepsPendingAuthenticationDeadline(t *testing.T) {
	_, key := shutdownHandshakeIdentity(t)
	var dispatched atomic.Bool
	fixture := newShutdownHandshakeFixture(t, &transport.Authentication{Key: key, Authorize: func(string) bool { return true }},
		func(context.Context, transport.Conn) error { dispatched.Store(true); return nil })
	ws, _, err := websocket.Dial(t.Context(), "ws://127.0.0.1/mesh", &websocket.DialOptions{ //nolint:bodyclose // websocket.Dial owns its response body
		HTTPClient: fixture.client, Subprotocols: []string{transport.AuthProtocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	fixture.cancel()
	waitSignal(t, fixture.listener.closed, "daemon listener shutdown")
	// The peer never starts TLS. The existing five-second authentication
	// deadline must release the upgraded connection and its group reservation.
	select {
	case err := <-fixture.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("stalled authentication retained the daemon beyond its handshake deadline")
	}
	if dispatched.Load() {
		t.Fatal("stalled authentication dispatched a handler")
	}
}

func TestDaemonShutdownStillCancelsAuthenticatedHandler(t *testing.T) {
	serverID, serverKey := shutdownHandshakeIdentity(t)
	clientID, clientKey := shutdownHandshakeIdentity(t)
	started, stopped := make(chan struct{}), make(chan struct{})
	fixture := newShutdownHandshakeFixture(t, &transport.Authentication{Key: serverKey, Authorize: func(id string) bool { return id == clientID }},
		func(ctx context.Context, _ transport.Conn) error {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil
		})
	conn, err := transport.DialOnce(t.Context(), "ws://127.0.0.1/mesh", transport.DialOptions{
		Auth: &transport.Authentication{Key: clientKey, ExpectedIdentity: serverID}, HTTPClient: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	waitSignal(t, started, "authenticated handler")
	fixture.cancel()
	waitSignal(t, stopped, "authenticated handler cancellation")
	if _, err := conn.ReadFrame(); err == nil {
		t.Fatal("authenticated connection survived daemon shutdown")
	}
	if err := waitRuntime(t, fixture.done); err != nil {
		t.Fatal(err)
	}
}

func testShutdownHandshake(t *testing.T, approved, current bool) {
	t.Helper()
	serverID, serverKey := shutdownHandshakeIdentity(t)
	clientID, clientKey := shutdownHandshakeIdentity(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	auth := &transport.Authentication{Key: serverKey, Authorize: func(id string) bool {
		enterOnce.Do(func() { close(entered) })
		<-release
		return approved && id == clientID
	}, Bind: func(string) transport.Authorization {
		return transport.Authorization{Class: access.FullDevice, Current: func() bool { return current }}
	}}
	var dispatched atomic.Bool
	fixture := newShutdownHandshakeFixture(t, auth, func(context.Context, transport.Conn) error {
		dispatched.Store(true)
		return nil
	})
	t.Cleanup(unblock)
	dialed := make(chan error, 1)
	go func() {
		conn, err := transport.DialOnce(t.Context(), "ws://127.0.0.1/mesh", transport.DialOptions{
			Auth: &transport.Authentication{Key: clientKey, ExpectedIdentity: serverID}, HTTPClient: fixture.client,
		})
		if conn != nil {
			assertRetiringHandshakeClosed(t, conn)
		}
		dialed <- err
	}()
	waitSignal(t, entered, "pending certificate authorization")
	fixture.cancel()
	waitSignal(t, fixture.listener.closed, "daemon listener shutdown")
	unblock()
	var err error
	select {
	case err = <-dialed:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("pending handshake did not finish")
	}
	if approved && current && err != nil {
		t.Fatalf("shutdown converted approved authentication into a terminal failure: %v", err)
	}
	if (!approved || !current) && !errors.Is(err, transport.ErrAuthentication) {
		t.Fatalf("denial or revocation lost its terminal authentication error: %v", err)
	}
	if dispatched.Load() {
		t.Fatal("pending authentication dispatched a handler after shutdown")
	}
	if err := waitRuntime(t, fixture.done); err != nil {
		t.Fatalf("daemon shutdown: %v", err)
	}
}

func assertRetiringHandshakeClosed(t *testing.T, conn transport.Conn) {
	t.Helper()
	if _, err := conn.ReadFrame(); err == nil {
		t.Error("retiring daemon left the authenticated connection open")
	}
	_ = conn.Close()
}

type shutdownHandshakeFixture struct {
	client   *http.Client
	cancel   context.CancelFunc
	listener *shutdownHandshakeListener
	done     chan error
}

func newShutdownHandshakeFixture(t *testing.T, auth *transport.Authentication, handler transport.Handler) *shutdownHandshakeFixture {
	t.Helper()
	server, peer := net.Pipe()
	listener := &shutdownHandshakeListener{pending: make(chan net.Conn, 1), closed: make(chan struct{})}
	listener.pending <- server
	unix := &shutdownHandshakeListener{pending: make(chan net.Conn), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	clientTransport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return peer, nil }}
	fixture := &shutdownHandshakeFixture{
		client: &http.Client{Transport: clientTransport}, cancel: cancel, listener: listener, done: make(chan error, 1),
	}
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
		_ = peer.Close()
		_ = listener.Close()
		_ = unix.Close()
		clientTransport.CloseIdleConnections()
		waitSignal(t, finished, "in-memory daemon cleanup")
	})
	go func() {
		defer close(finished)
		fixture.done <- serveBoundListeners(ctx, cancel, listenerConfig{
			controlAuth: auth, webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: time.Second,
		}, handler, unix, []net.Listener{listener}, nil, nil)
	}()
	return fixture
}

type shutdownHandshakeListener struct {
	pending chan net.Conn
	closed  chan struct{}
	once    sync.Once
}

func (l *shutdownHandshakeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.pending:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *shutdownHandshakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*shutdownHandshakeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7337}
}

func shutdownHandshakeIdentity(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public), private
}
