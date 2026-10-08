package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func TestServeBoundListenersClosesPreconnectsDuringShutdown(t *testing.T) {
	for _, surface := range []string{"tailnet", "tailnet-partial", "https", "https-handshake", "public", "public-tls", "public-tls-handshake"} {
		t.Run(surface, func(t *testing.T) {
			unixListener, _ := newTCPListener(t, "127.0.0.1:0")
			listener, _ := newTCPListener(t, "127.0.0.1:0")
			observed := &shutdownReadListener{Listener: listener, reading: make(chan struct{}), closing: make(chan struct{}), tlsReady: make(chan struct{})}
			certificatePEM, keyPEM := daemonTestCertificate(t, 123, time.Now())
			certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
			requests := make(chan struct{}, 1)
			config := listenerConfig{httpHandler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests <- struct{}{} }), webSocketPath: "/mesh", shutdownTimeout: httpShutdownTimeout, reporter: newErrorReporter(nil)}
			var tailnetListeners []net.Listener
			var httpsListener, publicListener net.Listener
			switch surface {
			case "tailnet", "tailnet-partial":
				tailnetListeners = []net.Listener{observed}
			case "https", "https-handshake":
				httpsListener, config.tlsConfig = observed, tlsConfig
			case "public":
				publicListener = observed
			case "public-tls", "public-tls-handshake":
				publicListener, config.publicTLSConfig = observed, tlsConfig
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() {
				done <- serveBoundListeners(ctx, cancel, config, func(context.Context, transport.Conn) error { return nil }, unixListener, tailnetListeners, httpsListener, publicListener)
			}()
			peer, err := net.DialTimeout("tcp", listener.Addr().String(), runtimeTestTimeout)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = peer.Close() })
			if surface == "https" || surface == "public-tls" {
				secure := tls.Client(peer, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}) //nolint:gosec // Self-signed loopback fixture.
				handshakeCtx, stopHandshake := context.WithTimeout(t.Context(), runtimeTestTimeout)
				defer stopHandshake()
				if err := secure.HandshakeContext(handshakeCtx); err != nil {
					t.Fatal(err)
				}
				waitSignal(t, observed.tlsReady, "TLS server handshake completion")
			}
			waitSignal(t, observed.reading, "HTTP connection read")
			if surface == "tailnet-partial" {
				if _, err := io.WriteString(peer, "GET /partial HTTP/1.1\r\nHost: 127.0.0.1\r\n"); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now()
			cancel()
			if surface == "tailnet-partial" {
				waitSignal(t, observed.closing, "HTTP listener shutdown")
				// Finishing headers after shutdown is unserved even without tracking.
				_, _ = io.WriteString(peer, "\r\n")
			}
			shutdownErr := waitRuntime(t, done)
			select {
			case <-requests:
				t.Fatal("an incomplete request was dispatched during shutdown")
			default:
			}
			if shutdownErr != nil {
				t.Fatal(shutdownErr)
			}
			if elapsed := time.Since(started); elapsed >= 1500*time.Millisecond {
				t.Fatalf("preconnect shutdown took %s, want less than 1.5s", elapsed)
			}
		})
	}
}

func TestNewHTTPConnectionsCloseLatePreconnectsAndChainHook(t *testing.T) {
	var states []http.ConnState
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout, ConnState: func(_ net.Conn, state http.ConnState) { states = append(states, state) }}
	connections := trackNewHTTPConnections(server)
	connections.closeNew()
	connection, peer := net.Pipe()
	defer connection.Close() //nolint:errcheck // Test cleanup.
	defer peer.Close()       //nolint:errcheck // Test cleanup.
	if err := peer.SetReadDeadline(time.Now().Add(runtimeTestTimeout)); err != nil {
		t.Fatal(err)
	}
	server.ConnState(connection, http.StateNew)
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("late preconnect read = %v, want EOF", err)
	}
	server.ConnState(connection, http.StateClosed)
	if len(states) != 2 || states[0] != http.StateNew || states[1] != http.StateClosed {
		t.Fatalf("previous state hook received %v", states)
	}
	connections.mu.Lock()
	defer connections.mu.Unlock()
	if len(connections.pending) != 0 {
		t.Fatalf("late preconnect left %d tracked connections", len(connections.pending))
	}
}

func TestNewHTTPConnectionsCloseTLSWithoutWaitingForPeer(t *testing.T) {
	certificatePEM, keyPEM := daemonTestCertificate(t, 124, time.Now())
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	connection, peer := net.Pipe()
	t.Cleanup(func() { _ = connection.Close() })
	t.Cleanup(func() { _ = peer.Close() })
	secure := tls.Server(connection, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}, SessionTicketsDisabled: true})
	client := tls.Client(peer, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}) //nolint:gosec // Self-signed pipe fixture.
	ctx, cancel := context.WithTimeout(t.Context(), runtimeTestTimeout)
	defer cancel()
	handshake := make(chan error, 1)
	go func() { handshake <- secure.HandshakeContext(ctx) }()
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-handshake; err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout}
	connections := trackNewHTTPConnections(server)
	server.ConnState(secure, http.StateNew)
	closed := make(chan struct{})
	go func() {
		connections.closeNew()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("TLS shutdown waited for the peer to read close-notify")
	}
	server.ConnState(secure, http.StateClosed)
}

func TestNewHTTPConnectionsPreserveActiveRequests(t *testing.T) {
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout}
	connections := trackNewHTTPConnections(server)
	connection, peer := net.Pipe()
	defer connection.Close() //nolint:errcheck // Test cleanup.
	defer peer.Close()       //nolint:errcheck // Test cleanup.
	server.ConnState(connection, http.StateNew)
	server.ConnState(connection, http.StateActive)
	connections.closeNew()
	if err := peer.SetWriteDeadline(time.Now().Add(runtimeTestTimeout)); err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() { _, err := io.ReadFull(connection, make([]byte, 1)); read <- err }()
	if _, err := peer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatalf("active request read: %v", err)
	}
	server.ConnState(connection, http.StateClosed)
}

type shutdownReadListener struct {
	net.Listener
	reading   chan struct{}
	closing   chan struct{}
	tlsReady  chan struct{}
	closeOnce sync.Once
}

func (l *shutdownReadListener) Close() error {
	l.closeOnce.Do(func() { close(l.closing) })
	if err := l.Listener.Close(); err != nil {
		return fmt.Errorf("shutdown fixture: close listener: %w", err)
	}
	return nil
}

//nolint:wrapcheck // net/http's accept retries require the original net.Error.
func (l *shutdownReadListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &shutdownReadConnection{Conn: connection, reading: l.reading, tlsReady: l.tlsReady}, nil
}

type shutdownReadConnection struct {
	net.Conn
	reading  chan struct{}
	tlsReady chan struct{}
	once     sync.Once
	tlsOnce  sync.Once
}

func (c *shutdownReadConnection) SetReadDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		// net/http restores the handshake deadline before reading HTTP headers.
		c.tlsOnce.Do(func() { close(c.tlsReady) })
	}
	if err := c.Conn.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("shutdown fixture: set read deadline: %w", err)
	}
	return nil
}

//nolint:wrapcheck // net/http distinguishes EOF and net.Error on connection reads.
func (c *shutdownReadConnection) Read(buffer []byte) (int, error) {
	c.once.Do(func() { close(c.reading) })
	return c.Conn.Read(buffer)
}
