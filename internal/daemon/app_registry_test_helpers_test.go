package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func appRegistryTestTLS(t *testing.T) *tls.Config {
	t.Helper()
	certPEM, keyPEM := daemonTestNamedCertificate(t, 17, time.Now(), "*.mesh.test")
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
}
func appRegistryTestDialProxy(ctx context.Context, address string) (net.Conn, error) {
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("dial app registry fixture: %w", err)
	}
	if _, err := io.WriteString(connection, "PROXY TCP4 100.64.0.2 127.0.0.1 40000 443\r\n"); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("write fixture PROXY header: %w", err)
	}
	return connection, nil
}
func appRegistryTestDialTLS(ctx context.Context, address string, serverTLS *tls.Config) (net.Conn, error) {
	connection, err := appRegistryTestDialProxy(ctx, address)
	if err != nil {
		return nil, err
	}
	clientTLS, err := appRegistryTestClientTLS(serverTLS)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	client := tls.Client(connection, clientTLS)
	if err := client.HandshakeContext(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("fixture TLS handshake: %w", err)
	}
	return client, nil
}
func appRegistryTestClient(t *testing.T, serverTLS *tls.Config) *http.Client {
	t.Helper()
	clientTLS, err := appRegistryTestClientTLS(serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: clientTLS, DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
		return appRegistryTestDialProxy(ctx, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: runtimeTestTimeout}
}

func appRegistryTestClientTLS(serverTLS *tls.Config) (*tls.Config, error) {
	certificate, err := x509.ParseCertificate(serverTLS.Certificates[0].Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse app registry fixture certificate: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "app.mesh.test"}, nil
}
