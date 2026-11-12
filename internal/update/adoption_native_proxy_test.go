package update

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	metadata "github.com/theupdateframework/go-tuf/v2/metadata"
)

type adoptionNativeProxy struct {
	server      *httptest.Server
	mu          sync.Mutex
	files       map[string][]byte
	connections map[net.Conn]*http.Server
	certificate tls.Certificate
}

func newAdoptionNativeProxy(t *testing.T, signer *signingFixture, root string) *adoptionNativeProxy {
	t.Helper()
	proxy := &adoptionNativeProxy{files: make(map[string][]byte), connections: make(map[net.Conn]*http.Server)}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(700), Subject: pkix.Name{CommonName: "Mesh fixture origin"}, DNSNames: []string{"github.com", "tuf-repo-cdn.sigstore.dev"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, signer.issuer, &key.PublicKey, signer.issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	proxy.certificate = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: signer.issuer.Raw})
	directory, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.WriteFile("fixture-ca.pem", ca, 0600); err != nil {
		t.Fatal(err)
	}
	proxy.installTUF(t, signer)
	proxy.server = httptest.NewServer(http.HandlerFunc(proxy.connect))
	t.Cleanup(func() {
		proxy.mu.Lock()
		for connection, server := range proxy.connections {
			_ = connection.Close()
			_ = server.Close()
		}
		proxy.mu.Unlock()
		proxy.server.Close()
	})
	return proxy
}

func (p *adoptionNativeProxy) put(name string, value []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.files[name] = value
}

func (p *adoptionNativeProxy) installTUF(t *testing.T, fixture *signingFixture) {
	t.Helper()
	var provenance struct {
		Witness struct {
			Snapshot []byte `json:"snapshot"`
			Targets  []struct {
				Name     string `json:"name"`
				Metadata []byte `json:"metadata"`
			} `json:"targets"`
		} `json:"witness"`
	}
	if err := json.Unmarshal(fixture.provenance, &provenance); err != nil {
		t.Fatal(err)
	}
	signer, err := signature.LoadSigner(fixture.issuerKey, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := metadata.Timestamp(time.Now().Add(time.Hour))
	timestamp.Signed.Meta["snapshot.json"] = metadata.MetaFile(1)
	p.put("tuf/timestamp.json", signFixtureTUF(t, timestamp, signer))
	p.put("tuf/1.snapshot.json", provenance.Witness.Snapshot)
	p.put("tuf/snapshot.json", provenance.Witness.Snapshot)
	for _, target := range provenance.Witness.Targets {
		p.put("tuf/1."+target.Name+".json", target.Metadata)
		p.put("tuf/"+target.Name+".json", target.Metadata)
	}
	p.put("tuf/trusted_root.json", fixture.trust.Trusted)
	p.put("tuf/"+discoveryDigest(fixture.trust.Trusted)+".trusted_root.json", fixture.trust.Trusted)
}

func (p *adoptionNativeProxy) connect(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodConnect || request.Host != "github.com:443" && request.Host != "tuf-repo-cdn.sigstore.dev:443" {
		http.Error(w, "fixture origin only", http.StatusForbidden)
		return
	}
	connection, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = connection.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		_ = connection.Close()
		return
	}
	secure := tls.Server(connection, &tls.Config{Certificates: []tls.Certificate{p.certificate}, MinVersion: tls.VersionTLS12})
	server := &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 5 * time.Second}
	p.mu.Lock()
	p.connections[connection] = server
	p.mu.Unlock()
	listener := &adoptionSingleConnection{connection: secure, closed: make(chan struct{})}
	_ = server.Serve(listener)
}

func (p *adoptionNativeProxy) serve(w http.ResponseWriter, request *http.Request) {
	name := strings.TrimPrefix(request.URL.Path, "/ShaulLavo/mesh/releases/download/")
	if request.Host == "tuf-repo-cdn.sigstore.dev" {
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		name = "tuf/" + parts[len(parts)-1]
	}
	p.mu.Lock()
	data, exists := p.files[name]
	p.mu.Unlock()
	if !exists {
		http.NotFound(w, request)
		return
	}
	_, _ = w.Write(data)
}

type adoptionSingleConnection struct {
	connection net.Conn
	once       sync.Once
	closeOnce  sync.Once
	closed     chan struct{}
}

func (listener *adoptionSingleConnection) Accept() (net.Conn, error) {
	var connection net.Conn
	listener.once.Do(func() { connection = listener.connection })
	if connection != nil {
		return connection, nil
	}
	<-listener.closed
	return nil, net.ErrClosed
}

func (listener *adoptionSingleConnection) Addr() net.Addr { return listener.connection.LocalAddr() }
func (listener *adoptionSingleConnection) Close() error {
	listener.closeOnce.Do(func() { close(listener.closed) })
	return hostError(listener.connection.Close())
}

func downloadAdoptionNativeArchive(ctx context.Context, client *http.Client, address string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, hostError(err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, hostError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("published fixture archive is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (128<<20)+1))
	if err != nil || len(data) > 128<<20 {
		return nil, errors.Join(errors.New("read bounded published fixture archive"), err)
	}
	return data, nil
}
