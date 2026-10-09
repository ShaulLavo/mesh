package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/transport"
)

func TestPrivateProxyListenerPreservesClientAndFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name, header, want                   string
		disallowed, rejected, forbidden, raw bool
	}{
		{name: "direct Serve IPv4", header: "PROXY TCP4 100.92.135.44 127.0.0.1 40000 8443\r\n", want: "100.92.135.44"},
		{name: "gateway IPv6", header: "PROXY TCP6 fd7a:115c:a1e0::1 ::1 40000 8443\r\n", want: "fd7a:115c:a1e0::1"},
		{name: "missing metadata", rejected: true},
		{name: "raw TLS option disabled", raw: true, forbidden: true},
		{name: "malformed metadata", header: "PROXY UNKNOWN\r\n", rejected: true},
		{name: "wrong UID", header: "PROXY TCP4 100.92.135.44 127.0.0.1 40000 8443\r\n", disallowed: true, rejected: true},
		{name: "loopback source", header: "PROXY TCP4 127.0.0.1 127.0.0.1 40000 8443\r\n", forbidden: true},
		{name: "unspecified source", header: "PROXY TCP4 0.0.0.0 127.0.0.1 40000 8443\r\n", forbidden: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reached := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached <- r.Header.Get("X-Forwarded-For")
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(backend.Close)
			_, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			services, err := serve.NewRegistryWithReservedPrefix([]serve.Service{{Name: "platform", Kind: serve.Proxy, Target: port}}, "/ws")
			if err != nil {
				t.Fatal(err)
			}
			listener, httpsPort := newTCPListener(t, "127.0.0.1:0")
			control, _ := newTCPListener(t, "127.0.0.1:0")
			certPEM, keyPEM := daemonTestNamedCertificate(t, 1, time.Now(), "host.example.test")
			cert, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(certPEM)
			cfg := ListenerConfig{StateDir: t.TempDir(), HTTPSPort: httpsPort, HTTPSProxyProtocol: !tt.raw, WebSocketPath: "/ws",
				PrivateName: func() string { return "host.example.test" }, HTTPHandler: services,
				TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }},
			}
			handler := func(context.Context, transport.Conn) error { return nil }
			normalized, err := validateListenerConfig(context.Background(), cfg, handler)
			if err != nil {
				t.Fatal(err)
			}
			if normalized.httpsProxyProtocol == tt.raw {
				t.Fatal("private PROXY setting was lost")
			}
			if tt.disallowed {
				normalized.proxyForwarderUIDs = []uint32{proxyTestUID(t) + 1}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- serveBoundListeners(ctx, cancel, normalized, handler, control, nil, listener, nil) }()
			t.Cleanup(func() {
				cancel()
				if err := waitRuntime(t, done); err != nil {
					t.Error(err)
				}
			})
			client, err := net.Dial("tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			_ = client.SetDeadline(time.Now().Add(runtimeTestTimeout))
			if tt.header != "" {
				if _, err := io.WriteString(client, tt.header); err != nil {
					t.Fatal(err)
				}
			}
			secured := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "host.example.test"})
			err = secured.Handshake()
			if tt.rejected {
				if err == nil {
					t.Fatal("unverified relay completed TLS")
				}
				select {
				case got := <-reached:
					t.Fatalf("rejected relay reached backend as %s", got)
				default:
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.WriteString(secured, "GET /platform/pairing/status HTTP/1.1\r\nHost: host.example.test\r\nX-Forwarded-For: 127.0.0.1\r\nX-Forwarded-Proto: https\r\nForwarded: for=127.0.0.1\r\nConnection: close\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(secured), &http.Request{Method: http.MethodGet})
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if tt.forbidden {
				if response.StatusCode != http.StatusForbidden {
					t.Fatalf("invalid source status = %d", response.StatusCode)
				}
				select {
				case got := <-reached:
					t.Fatalf("invalid source reached backend as %s", got)
				default:
				}
				return
			}
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("response = %d", response.StatusCode)
			}
			if got := <-reached; got != tt.want {
				t.Fatalf("backend client = %q, want %q", got, tt.want)
			}
		})
	}
}
