package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestCoalescedHTTP2ConnectionRejectsSecondHostBeforeDispatch(t *testing.T) {
	registry := testRegistry(t, ModeDirectTLS, time.Now())
	t.Cleanup(registry.Close)
	var calls atomic.Int32
	registry.SetAppHandler(appHandlerFunc(func(w http.ResponseWriter, r *http.Request, name string) bool {
		calls.Add(1)
		if name == "ce8z.mesh.test" {
			http.NotFound(w, r)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}))
	server := httptest.NewUnstartedServer(registry)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{coalescingCertificate(t)}}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http2.Transport{}
	connect := func(host string) *http2.ClientConn {
		t.Helper()
		connection, err := tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{
			RootCAs: roots, ServerName: host, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		if connection.ConnectionState().NegotiatedProtocol != "h2" {
			t.Fatal("test connection did not negotiate HTTP/2")
		}
		client, err := transport.NewClientConn(connection)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	request := func(client *http2.ClientConn, host string, want int, wantCalls int32) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.ProtoMajor != 2 || response.StatusCode != want || calls.Load() != wantCalls {
			t.Fatalf("host %s: protocol=%s status=%d dispatches=%d; want HTTP/2 status=%d dispatches=%d", host, response.Proto, response.StatusCode, calls.Load(), want, wantCalls)
		}
		if want == http.StatusMisdirectedRequest && (len(body) != 0 || response.Header.Get("Content-Type") != "") {
			t.Fatalf("coalesced host %s returned renderable content: %q, type=%q", host, string(body), response.Header.Get("Content-Type"))
		}
	}
	shared := connect("saha.mesh.test")
	request(shared, "saha.mesh.test", http.StatusNoContent, 1)
	request(shared, "ce8z.mesh.test", http.StatusMisdirectedRequest, 1)
	request(shared, "saha.mesh.test", http.StatusNoContent, 2)
	// A fresh connection reaches the actual host's handler, which still denies access.
	request(connect("ce8z.mesh.test"), "ce8z.mesh.test", http.StatusNotFound, 3)
}

func coalescingCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"*.mesh.test"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
