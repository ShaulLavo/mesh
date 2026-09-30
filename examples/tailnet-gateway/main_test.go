package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGatewayPreservesTLSAndRoutesByServerName(t *testing.T) {
	hosts := []string{"apps.shaulavo.dev", "5yfw.shaulavo.dev", "omarchy.mesh.shaulavo.dev", "longer.shaulavo.dev", "iiii.shaulavo.dev"}
	certificate, roots := gatewayCertificate(t, hosts)
	backend := func(name string) *httptest.Server {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(name + ":" + r.TLS.ServerName + ":" + string(body))
		}))
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
		server.StartTLS()
		t.Cleanup(server.Close)
		return server
	}
	private, apps := backend("private"), backend("apps")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go acceptGateway(listener, private.Listener.Addr().String(), apps.Listener.Addr().String())
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, host := range hosts {
			checkGatewayRequest(t, listener.Addr().String(), host, version, roots)
		}
	}
}

func acceptGateway(listener net.Listener, private, apps string) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		go bridge(client, private, apps)
	}
}

func checkGatewayRequest(t *testing.T, address, host string, version uint16, roots *x509.CertPool) {
	t.Helper()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: version, MaxVersion: version, RootCAs: roots},
	}
	transport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		return net.DialTimeout("tcp", address, time.Second)
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	payload := strings.Repeat("encrypted body ", 8192)
	response, err := client.Post("https://"+host+"/", "text/plain", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("TLS %x for %s: %v", version, host, err)
	}
	defer func() { _ = response.Body.Close() }()
	var body string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	target := "private"
	if host == "apps.shaulavo.dev" || host == "5yfw.shaulavo.dev" {
		target = "apps"
	}
	if response.TLS.Version != version || body != target+":"+host+":"+payload {
		t.Fatalf("TLS %x request for %s reached the wrong backend or lost bytes", version, host)
	}
}

func gatewayCertificate(t *testing.T, hosts []string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: hosts,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}, roots
}
