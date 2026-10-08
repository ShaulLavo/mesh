package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func TestPublicTLSOffersHTTP1ForSeparateHostConnections(t *testing.T) {
	unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unixListener.Close() })
	publicListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publicListener.Close() })
	certificatePEM, keyPEM := daemonTestNamedCertificate(t, 1, time.Now(), "*.mesh.test")
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(ctx, cancel, listenerConfig{
			webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: time.Second,
			publicTLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"h2", "http/1.1"}},
			publicHTTPHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != r.TLS.ServerName {
					t.Errorf("host %s arrived on SNI %s", r.Host, r.TLS.ServerName)
				}
				w.WriteHeader(http.StatusNoContent)
			}),
		}, func(context.Context, transport.Conn) error { return nil }, unixListener, nil, nil, publicListener)
	}()
	t.Cleanup(func() {
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Error(err)
		}
	})
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certificatePEM)
	clientTransport := &http.Transport{
		ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, publicListener.Addr().String())
		},
	}
	t.Cleanup(clientTransport.CloseIdleConnections)
	client := &http.Client{Transport: clientTransport, Timeout: runtimeTestTimeout}
	for _, host := range []string{"saha.mesh.test", "ce8z.mesh.test", "saha.mesh.test"} {
		response, err := client.Get("https://" + host + "/")
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent || response.ProtoMajor != 1 || response.TLS.NegotiatedProtocol != "http/1.1" {
			t.Fatalf("host %s: status=%d protocol=%s ALPN=%q; want 204 over HTTP/1.1", host, response.StatusCode, response.Proto, response.TLS.NegotiatedProtocol)
		}
	}
}
