package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func TestPublicOwnerAccessListenerAuthenticatesBeforeTLS(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		name := "disallowed"
		if allowed {
			name = "allowed"
		}
		t.Run(name, func(t *testing.T) {
			publicListener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = publicListener.Close() })
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
				StateDir: t.TempDir(), PublicListenAddress: publicListener.Addr().String(), TailnetOwnerAccess: true,
				PublicHTTPHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-Test-Remote-Addr", r.RemoteAddr)
					w.WriteHeader(http.StatusNoContent)
				}),
				PublicTLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					return &cert, nil
				}},
			}
			handler := func(context.Context, transport.Conn) error { return nil }
			normalized, err := validateListenerConfig(context.Background(), cfg, handler)
			if err != nil {
				t.Fatal(err)
			}
			if !allowed {
				normalized.proxyForwarderUIDs = []uint32{uint32(os.Getuid()) + 1}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- serveBoundListeners(ctx, cancel, normalized, handler, unixListener, nil, nil, publicListener)
			}()
			t.Cleanup(func() {
				cancel()
				if err := waitRuntime(t, done); err != nil {
					t.Error(err)
				}
			})
			client, err := net.Dial("tcp4", publicListener.Addr().String())
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
