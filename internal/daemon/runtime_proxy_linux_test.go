package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func TestAppRegistryOwnerAccessListenerAuthenticatesBeforeTLS(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		name := "disallowed"
		if allowed {
			name = "allowed"
		}
		t.Run(name, func(t *testing.T) {
			appRegistryListener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = appRegistryListener.Close() })
			unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = unixListener.Close() })
			certPEM, keyPEM := daemonTestNamedCertificate(t, 1, time.Now(), "apps.example.test")
			cert, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(certPEM) {
				t.Fatal("test certificate could not be trusted")
			}
			cfg := ListenerConfig{
				StateDir: t.TempDir(), AppRegistryListenAddress: appRegistryListener.Addr().String(),
				AppRegistryHTTPHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-Test-Remote-Addr", r.RemoteAddr)
					w.WriteHeader(http.StatusNoContent)
				}),
				AppRegistryTLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					return &cert, nil
				}},
			}
			handler := func(context.Context, transport.Conn) error { return nil }
			normalized, err := validateListenerConfig(context.Background(), cfg, handler)
			if err != nil {
				t.Fatal(err)
			}
			if !allowed {
				normalized.proxyForwarderUIDs = []uint32{proxyTestUID(t) + 1}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- serveBoundListeners(ctx, cancel, normalized, handler, unixListener, nil, nil, appRegistryListener)
			}()
			t.Cleanup(func() {
				cancel()
				if err := waitRuntime(t, done); err != nil {
					t.Error(err)
				}
			})
			client, err := net.Dial("tcp4", appRegistryListener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			_ = client.SetDeadline(time.Now().Add(runtimeTestTimeout))
			if _, err := io.WriteString(client, "PROXY TCP4 100.64.0.2 127.0.0.1 40000 443\r\n"); err != nil {
				t.Fatal(err)
			}
			tlsClient := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "apps.example.test"})
			err = tlsClient.Handshake()
			if !allowed {
				if err == nil {
					t.Fatal("daemon admitted a disallowed forwarder to TLS")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(tlsClient, "GET / HTTP/1.1\r\nHost: apps.example.test\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(tlsClient), &http.Request{Method: http.MethodGet})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusNoContent || response.Header.Get("X-Test-Remote-Addr") != "100.64.0.2:40000" {
				t.Fatalf("daemon lost authenticated source: status %d, address %s", response.StatusCode, response.Header.Get("X-Test-Remote-Addr"))
			}
		})
	}
}

func TestAppRegistryProxyAuthenticationPoolExpiresStalledHeaders(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedAppRegistryListener(base, maximumAppRegistryConnections)
	listener.proxyUIDs = []uint32{proxyTestUID(t)}
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout, ConnState: listener.connState,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.closeActive() })
	stalled := make([]net.Conn, 0, maximumAppRegistryAuthenticating)
	for range maximumAppRegistryAuthenticating {
		connection, err := net.Dial("tcp4", base.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		stalled = append(stalled, connection)
		t.Cleanup(func() { _ = connection.Close() })
	}
	deadline := time.Now().Add(time.Second)
	for {
		listener.mu.Lock()
		pending := len(listener.active)
		listener.mu.Unlock()
		if pending == maximumAppRegistryAuthenticating {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("authentication pool did not fill")
		}
		<-time.After(time.Millisecond)
	}
	connection, err := net.Dial("tcp4", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(appRegistryProxyHeaderTimeout + time.Second))
	if _, err := io.WriteString(connection, "PROXY TCP4 198.51.100.2 127.0.0.1 40000 443\r\nGET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("stalled authentication sockets starved a valid forwarder: %v", err)
	}
	_ = response.Body.Close()
	for _, socket := range stalled {
		_ = socket.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := socket.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("stalled PROXY socket read = %v, want EOF", err)
		}
	}
}

func TestAppRegistryProxyQuotaUsesAuthenticatedSource(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedAppRegistryListener(base, maximumAppRegistryConnections)
	listener.proxyUIDs = []uint32{proxyTestUID(t)}
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout, ConnState: listener.connState,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Source", r.RemoteAddr)
			w.WriteHeader(http.StatusNoContent)
		})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.closeActive() })
	for i := range maximumAppRegistrySourceConnections + 2 {
		connection, err := net.Dial("tcp4", base.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		_ = connection.SetDeadline(time.Now().Add(time.Second))
		source := "198.51.100.1"
		if i == maximumAppRegistrySourceConnections+1 {
			source = "198.51.100.2"
		}
		if _, err := fmt.Fprintf(connection, "PROXY TCP4 %s 127.0.0.1 40000 443\r\nGET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n", source); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
		if i == maximumAppRegistrySourceConnections {
			if err == nil {
				_ = response.Body.Close()
				t.Fatal("over-share authenticated source was admitted")
			}
			continue
		}
		if err != nil {
			t.Fatalf("source %s request %d: %v", source, i, err)
		}
		_ = response.Body.Close()
		if got := response.Header.Get("X-Source"); got != source+":40000" {
			t.Fatalf("source = %s, want %s", got, source)
		}
	}
}

func TestAppRegistryFailedProxyDoesNotEvictAuthenticatedIdle(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedAppRegistryListener(base, 1)
	listener.proxyUIDs = []uint32{proxyTestUID(t)}
	idle := make(chan struct{}, 2)
	closed := make(chan struct{}, 2)
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout, ConnState: func(c net.Conn, state http.ConnState) {
		listener.connState(c, state)
		if state == http.StateIdle {
			idle <- struct{}{}
		}
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.closeActive() })
	legitimate, err := net.Dial("tcp4", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legitimate.Close() }()
	_ = legitimate.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(legitimate, "PROXY TCP4 198.51.100.1 127.0.0.1 40000 443\r\nGET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(legitimate)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	waitSignal(t, idle, "authenticated idle keep-alive")
	invalid, err := net.Dial("tcp4", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = invalid.Close() }()
	if _, err := io.WriteString(invalid, "NOT-A-PROXY-HEADER\r\n"); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, closed, "failed PROXY connection")
	_ = legitimate.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(legitimate, "GET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n"); err != nil {
		t.Fatalf("failed authentication evicted legitimate socket: %v", err)
	}
	response, err = http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("failed authentication evicted legitimate socket: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("legitimate keep-alive status %d", response.StatusCode)
	}
}

func TestAppRegistryDisallowedProxyNeverAdmits(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedAppRegistryListener(base, 1)
	listener.proxyUIDs = []uint32{proxyTestUID(t) + 1}
	t.Cleanup(func() { _ = listener.Close(); _ = listener.closeActive() })
	peer, err := net.Dial("tcp4", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	connection, err := listener.acceptTracked()
	if err != nil {
		t.Fatal(err)
	}
	connection.RemoteAddr()
	listener.mu.Lock()
	admitted := listener.admitted
	pending := connection.pending
	listener.mu.Unlock()
	if admitted != 0 || pending {
		t.Fatalf("disallowed forwarder admitted=%d pending=%t, want 0 and false", admitted, pending)
	}
}

func TestAppRegistryListenerRejectsUnprotectedRoles(t *testing.T) {
	getter := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }
	cases := []ListenerConfig{
		{AppRegistryListenAddress: "0.0.0.0:443", AppRegistryHTTPHandler: http.NotFoundHandler(), AppRegistryTLSConfig: &tls.Config{GetCertificate: getter}},
		{AppRegistryListenAddress: "203.0.113.4:443", AppRegistryHTTPHandler: http.NotFoundHandler(), AppRegistryTLSConfig: &tls.Config{GetCertificate: getter}},
		{AppRegistryListenAddress: "127.0.0.1:8445", AppRegistryHTTPHandler: http.NotFoundHandler()},
		{AppRegistryListenAddress: "127.0.0.1:8445", AppRegistryTLSConfig: &tls.Config{GetCertificate: getter}},
		{AppRegistryListenAddress: "127.0.0.1:8445", AppRegistryHTTPHandler: http.NotFoundHandler(), AppRegistryTLSConfig: &tls.Config{}},
		{AppRegistryListenAddress: "127.0.0.1:8445", AppRegistryHTTPHandler: http.NotFoundHandler(), AppRegistryTLSConfig: &tls.Config{MinVersion: tls.VersionTLS11, GetCertificate: getter}}, //nolint:gosec // old-TLS rejection fixture
		{AppRegistryHTTPHandler: http.NotFoundHandler(), AppRegistryTLSConfig: &tls.Config{GetCertificate: getter}},
	}
	for index, cfg := range cases {
		cfg.StateDir = t.TempDir()
		if _, err := validateListenerConfig(context.Background(), cfg, func(context.Context, transport.Conn) error { return nil }); err == nil {
			t.Fatalf("unprotected app registry config %d accepted", index)
		}
	}
}
