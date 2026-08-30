package dnsname

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBundleStorePublishesComplete0600Versions(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	store, err := NewBundleStore(filepath.Join(t.TempDir(), "certificates"), WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	certificatePEM, keyPEM := testCertificate(t, 1, WildcardName, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	bundle, err := store.Install(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Fingerprint != bundle.Fingerprint || !loaded.NotAfter.Equal(bundle.NotAfter) {
		t.Fatalf("loaded bundle = %#v, installed = %#v", loaded, bundle)
	}
	for _, path := range []string{
		filepath.Join(store.root, currentBundleFile),
		filepath.Join(store.root, bundle.Fingerprint, certificateFile),
		filepath.Join(store.root, bundle.Fingerprint, privateKeyFile),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %04o, want 0600", path, info.Mode().Perm())
		}
	}
}

func TestBundleStoreRejectsLoosePermissions(t *testing.T) {
	root := t.TempDir()
	store, err := NewBundleStore(root, WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, currentBundleFile), []byte("bad\n"), 0o644); err != nil { //nolint:gosec // deliberately loose permissions are the rejection fixture
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("loosely permissioned pointer loaded")
	}
}

func TestReadSecureFileRejectsSymlinkAndOversizedDescriptor(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureFile(link, 32); err == nil {
		t.Fatalf("secure symlink error = %v", err)
	}
	fifo := filepath.Join(directory, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := readSecureFile(fifo, 32); err == nil || !strings.Contains(err.Error(), "not regular") {
		t.Fatalf("secure FIFO error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("secure FIFO read blocked for %s", elapsed)
	}

	oversized := filepath.Join(directory, "oversized")
	if err := os.WriteFile(oversized, make([]byte, 33), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureFile(oversized, 32); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("oversized secure file error = %v", err)
	}
}

func TestCertificateSourceHotReloads(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	store, err := NewBundleStore(t.TempDir(), WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	source, err := NewCertificateSource(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.GetCertificate(nil); !errors.Is(err, ErrNoCertificate) {
		t.Fatalf("empty source error = %v", err)
	}
	firstCert, firstKey := testCertificate(t, 1, WildcardName, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	first, err := source.Install(firstCert, firstKey)
	if err != nil {
		t.Fatal(err)
	}
	secondCert, secondKey := testCertificate(t, 2, WildcardName, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	second, err := source.Install(secondCert, secondKey)
	if err != nil {
		t.Fatal(err)
	}
	current, err := source.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == second.Fingerprint || current.Leaf.SerialNumber.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("first = %s, second = %s, current serial = %s", first.Fingerprint, second.Fingerprint, current.Leaf.SerialNumber)
	}
}

func TestCertificateSourceStartsEmptyWhenPersistedCertificateExpired(t *testing.T) {
	root := t.TempDir()
	installedAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	store, err := NewBundleStore(root, WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return installedAt }
	certificatePEM, keyPEM := testCertificate(t, 1, WildcardName, installedAt.Add(-time.Hour), installedAt.Add(time.Hour))
	if _, err := store.Install(certificatePEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return installedAt.Add(2 * time.Hour) }
	source, err := NewCertificateSource(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.GetCertificate(nil); !errors.Is(err, ErrNoCertificate) {
		t.Fatalf("expired source error = %v, want ErrNoCertificate", err)
	}
}

func testCertificate(t *testing.T, serial int64, name string, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
