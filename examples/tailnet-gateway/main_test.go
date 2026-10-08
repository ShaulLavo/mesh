package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

func proxyTestUID(t *testing.T) uint32 {
	t.Helper()
	uid := int64(os.Getuid())
	if uid >= 0 && uid <= math.MaxUint32 {
		return uint32(uid)
	}
	t.Fatalf("test process UID %d is outside the kernel UID range", uid)
	return 0
}

func TestGatewayPreservesTLSAndRoutesByServerName(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprint(metadata), func(t *testing.T) { checkGateway(t, metadata) })
	}
}

func checkGateway(t *testing.T, metadata bool) {
	if metadata && runtime.GOOS != "linux" {
		if _, err := tailnet.ProxyForwarderUIDs(); err == nil {
			t.Fatal("owner access enabled without peer UID authentication")
		}
		return
	}
	hosts := []string{"apps.mesh.test", "5yfw.mesh.test", "omarchy.mesh.mesh.test", "longer.mesh.test", "iiii.mesh.test", "fregat.mesh.test"}
	certificate, roots := gatewayCertificate(t, hosts)
	backend := func(name string) *httptest.Server {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if metadata && r.RemoteAddr != "100.64.0.2:12345" {
				t.Errorf("lost verified device address: %s", r.RemoteAddr)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(name + ":" + r.TLS.ServerName + ":" + string(body))
		}))
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
		if metadata {
			server.Listener = tailnet.ProxyListener{Listener: server.Listener, AllowedUIDs: []uint32{proxyTestUID(t)}}
		}
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
	if metadata {
		listener = tailnet.ProxyListener{Listener: listener, AllowedUIDs: []uint32{proxyTestUID(t)}}
	}
	go acceptGateway(listener, private.Listener.Addr().String(), apps.Listener.Addr().String(), metadata)
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, host := range hosts {
			checkGatewayRequest(t, listener.Addr().String(), host, version, roots, metadata)
		}
	}
}

func acceptGateway(listener net.Listener, private, apps string, metadata bool) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		go bridge(client, private, apps, metadata)
	}
}

func checkGatewayRequest(t *testing.T, address, host string, version uint16, roots *x509.CertPool, metadata bool) {
	t.Helper()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: version, MaxVersion: version, RootCAs: roots},
	}
	transport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil && metadata {
			_, err = io.WriteString(connection, "PROXY TCP4 100.64.0.2 127.0.0.1 12345 443\r\n")
			if err != nil {
				_ = connection.Close()
			}
		}
		return connection, err
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	payload := strings.Repeat("encrypted body ", 8192)
	response, err := client.Post("https://"+host+"/", "text/plain", strings.NewReader(payload))
	if !metadata && !appHost(host) {
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("private gateway route accepted unverified client metadata")
		}
		return
	}
	if err != nil {
		t.Fatalf("TLS %x for %s: %v", version, host, err)
	}
	defer func() { _ = response.Body.Close() }()
	var body string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	target := "private"
	if host == "apps.mesh.test" || host == "5yfw.mesh.test" {
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

func TestAppHostAcceptsBothConfiguredDomains(t *testing.T) {
	for _, host := range []string{"apps.mesh.test", "7k3d.mesh.test", "apps.old.test", "7k3d.old.test"} {
		if !appHost(host) {
			t.Fatalf("configured app host rejected: %s", host)
		}
	}
	for _, host := range []string{"apps.other.test", "7k3d.old.test.evil", "pc.mesh.old.test"} {
		if appHost(host) {
			t.Fatalf("unconfigured app host accepted: %s", host)
		}
	}
}

func TestAppHostCanonicalSNI(t *testing.T) {
	for _, host := range []string{"APPS.OLD.TEST.", "7K3D.MESH.TEST."} {
		if !appHost(host) {
			t.Fatalf("canonical app SNI missed: %s", host)
		}
	}
}
