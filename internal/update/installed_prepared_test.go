//go:build mesh_installed_fixture

package update

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only the HTTPS serving leaf key crosses the job boundary. Issuer, Sigstore,
// transparency log and TUF signing keys never leave the preparation process.
type preparedOrigin struct {
	Certificate []byte            `json:"certificate"`
	ServingKey  []byte            `json:"servingKey"`
	Files       map[string][]byte `json:"files"`
}

func exportPreparedOrigin(t *testing.T, root *os.Root, proxy *adoptionNativeProxy, name string) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(proxy.certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	value := preparedOrigin{Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.certificate.Certificate[0]}),
		ServingKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), Files: make(map[string][]byte)}
	proxy.mu.Lock()
	for path, data := range proxy.files {
		if strings.HasPrefix(path, "tuf/") {
			value.Files[path] = data
		}
	}
	proxy.mu.Unlock()
	installedWrite(t, root, name, mustHostJSON(value))
}

func loadPreparedOrigin(t *testing.T, root *os.Root) *adoptionNativeProxy {
	t.Helper()
	data, err := root.ReadFile("prepared-origin.json")
	if err != nil {
		t.Fatal(err)
	}
	var value preparedOrigin
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(value.Certificate, value.ServingKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || leaf.IsCA || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		t.Fatalf("prepared HTTPS serving leaf is invalid or expired: %v", err)
	}
	for name := range value.Files {
		if !strings.HasPrefix(name, "tuf/") || !filepath.IsLocal(name) {
			t.Fatal("prepared origin contains a non-TUF asset")
		}
	}
	proxy := &adoptionNativeProxy{files: value.Files, certificate: certificate, connections: make(map[net.Conn]*http.Server)}
	proxy.server = httptest.NewServer(http.HandlerFunc(proxy.connect))
	t.Cleanup(func() { closePreparedOrigin(proxy) })
	installedWrite(t, root, "proxy.json", mustHostJSON(map[string]string{"address": proxy.server.URL}))
	return proxy
}

func closePreparedOrigin(proxy *adoptionNativeProxy) {
	proxy.mu.Lock()
	for connection, server := range proxy.connections {
		_ = connection.Close()
		_ = server.Close()
	}
	proxy.mu.Unlock()
	proxy.server.Close()
}

// This entry point loads immutable evidence and serves it. It never constructs
// a signing fixture, generates keys, or restores a live installation journal.
func TestInstalledPreparedServe(t *testing.T) {
	directory := os.Getenv("MESH_INSTALLED_PREPARED_DIR")
	if directory == "" {
		t.Skip("prepared origin requires its owned consumer scratch directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	proxy := loadPreparedOrigin(t, root)
	t.Setenv("SSL_CERT_FILE", filepath.Join(directory, "fixture-ca.pem"))
	t.Setenv("SSL_CERT_DIR", filepath.Join(directory, "empty-certs"))
	t.Setenv("HTTPS_PROXY", proxy.server.URL)
	t.Setenv("HTTP_PROXY", proxy.server.URL)
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	listener, err := net.Listen("unix", filepath.Join(directory, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	for {
		connection, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if installedSignConnection(t, root, nil, proxy, connection) {
			return
		}
	}
}

func TestPreparedOriginRoundTrip(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	fixture := newSigningFixture(t)
	producer := newAdoptionNativeProxy(t, fixture, directory)
	producer.put("private-unexported", []byte("only immutable TUF assets cross this boundary"))
	exportPreparedOrigin(t, root, producer, "prepared-origin.json")
	consumer := loadPreparedOrigin(t, root)
	if len(consumer.files) == 0 || consumer.files["private-unexported"] != nil {
		t.Fatal("prepared origin did not preserve exactly its TUF asset set")
	}
	leaf, err := x509.ParseCertificate(consumer.certificate.Certificate[0])
	if err != nil || leaf.IsCA || bytes.Equal(leaf.RawSubjectPublicKeyInfo, fixture.issuer.RawSubjectPublicKeyInfo) {
		t.Fatalf("exported material is not the distinct HTTPS serving leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(fixture.issuer)
	address, err := url.Parse(consumer.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(address), TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("https://tuf-repo-cdn.sigstore.dev/timestamp.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	actual, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(actual, producer.files["tuf/timestamp.json"]) || response.StatusCode != http.StatusOK {
		t.Fatalf("prepared origin changed its actual HTTPS TUF bytes: %v", err)
	}
	assertPreparedSigningRejected(t, root, consumer)
}

func assertPreparedSigningRejected(t *testing.T, root *os.Root, proxy *adoptionNativeProxy) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan bool, 1)
	go func() { done <- installedSignConnection(t, root, nil, proxy, server) }()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(client).Encode(installedSignRequest{Files: []string{"trusted-root.json"}, Output: "forbidden.json"}); err != nil {
		t.Fatal(err)
	}
	var reply map[string]string
	if err := json.NewDecoder(client).Decode(&reply); err != nil || reply["error"] == "" || <-done {
		t.Fatalf("prepared origin accepted signing: %v %v", reply, err)
	}
	if _, err := root.Stat("forbidden.json"); !os.IsNotExist(err) {
		t.Fatal("prepared origin wrote a new signature")
	}
}
