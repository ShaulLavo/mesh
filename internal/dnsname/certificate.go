package dnsname

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maximumCertificatePEM = 1 << 20
	maximumPrivateKeyPEM  = 64 << 10
	currentBundleFile     = "current"
	certificateFile       = "fullchain.pem"
	privateKeyFile        = "private.key"
)

var (
	ErrNoCertificate          = errors.New("dnsname: no certificate installed")
	ErrCertificateNotYetValid = errors.New("dnsname: certificate is not valid yet")
	ErrCertificateExpired     = errors.New("dnsname: certificate expired")
)

// Bundle is one validated certificate chain and its private key.
type Bundle struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	Fingerprint    string
	NotBefore      time.Time
	NotAfter       time.Time
	TLSCertificate tls.Certificate
}

// BundleStore publishes a complete certificate and key through one atomic
// current-version pointer. A crash cannot expose a mismatched pair.
type BundleStore struct {
	root         string
	expectedName string
	now          func() time.Time
	mu           sync.Mutex
}

// NewBundleStore creates a store rooted at root for expectedName.
func NewBundleStore(root, expectedName string) (*BundleStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("dnsname: certificate store root is empty")
	}
	if _, err := certificateProbeName(expectedName); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("dnsname: resolve certificate store %s: %w", root, err)
	}
	return &BundleStore{root: absolute, expectedName: expectedName, now: time.Now}, nil
}

// Load returns the current complete certificate and key.
func (s *BundleStore) Load() (Bundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Install validates and atomically publishes a complete certificate and key.
func (s *BundleStore) Install(certificatePEM, privateKeyPEM []byte) (Bundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bundle, err := ValidateBundle(certificatePEM, privateKeyPEM, s.expectedName, s.now().UTC())
	if err != nil {
		return Bundle{}, err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return Bundle{}, fmt.Errorf("dnsname: create certificate store %s: %w", s.root, err)
	}
	if err := os.Chmod(s.root, 0o700); err != nil {
		return Bundle{}, fmt.Errorf("dnsname: secure certificate store %s: %w", s.root, err)
	}
	versionDir := filepath.Join(s.root, bundle.Fingerprint)
	if _, err := os.Stat(versionDir); errors.Is(err, os.ErrNotExist) {
		if err := s.writeVersion(versionDir, bundle); err != nil {
			return Bundle{}, err
		}
	} else if err != nil {
		return Bundle{}, fmt.Errorf("dnsname: inspect certificate version %s: %w", bundle.Fingerprint, err)
	} else {
		existing, err := s.loadVersion(bundle.Fingerprint)
		if err != nil {
			return Bundle{}, fmt.Errorf("dnsname: validate existing certificate version %s: %w", bundle.Fingerprint, err)
		}
		bundle = existing
	}
	if err := writeAtomicFile(filepath.Join(s.root, currentBundleFile), []byte(bundle.Fingerprint+"\n"), 0o600); err != nil {
		return Bundle{}, fmt.Errorf("dnsname: publish certificate version %s: %w", bundle.Fingerprint, err)
	}
	if err := s.removeOldVersions(bundle.Fingerprint); err != nil {
		return Bundle{}, err
	}
	return cloneBundle(bundle), nil
}

func (s *BundleStore) loadLocked() (Bundle, error) {
	currentPath := filepath.Join(s.root, currentBundleFile)
	contents, err := readSecureFile(currentPath, 128)
	if errors.Is(err, os.ErrNotExist) {
		return Bundle{}, ErrNoCertificate
	}
	if err != nil {
		return Bundle{}, err
	}
	fingerprint := strings.TrimSpace(string(contents))
	if len(fingerprint) != sha256.Size*2 {
		return Bundle{}, fmt.Errorf("dnsname: certificate pointer %s is invalid", currentPath)
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return Bundle{}, fmt.Errorf("dnsname: certificate pointer %s is invalid: %w", currentPath, err)
	}
	return s.loadVersion(fingerprint)
}

func (s *BundleStore) loadVersion(fingerprint string) (Bundle, error) {
	directory := filepath.Join(s.root, fingerprint)
	certificatePEM, err := readSecureFile(filepath.Join(directory, certificateFile), maximumCertificatePEM)
	if err != nil {
		return Bundle{}, err
	}
	privateKeyPEM, err := readSecureFile(filepath.Join(directory, privateKeyFile), maximumPrivateKeyPEM)
	if err != nil {
		return Bundle{}, err
	}
	bundle, err := ValidateBundle(certificatePEM, privateKeyPEM, s.expectedName, s.now().UTC())
	if err != nil {
		return Bundle{}, err
	}
	if bundle.Fingerprint != fingerprint {
		return Bundle{}, fmt.Errorf("dnsname: certificate directory %s contains fingerprint %s", fingerprint, bundle.Fingerprint)
	}
	return bundle, nil
}

func (s *BundleStore) writeVersion(versionDir string, bundle Bundle) error {
	temporary, err := os.MkdirTemp(s.root, ".certificate-*")
	if err != nil {
		return fmt.Errorf("dnsname: create certificate staging directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return fmt.Errorf("dnsname: secure certificate staging directory: %w", err)
	}
	if err := writeNewFile(filepath.Join(temporary, certificateFile), bundle.CertificatePEM, 0o600); err != nil {
		return err
	}
	if err := writeNewFile(filepath.Join(temporary, privateKeyFile), bundle.PrivateKeyPEM, 0o600); err != nil {
		return err
	}
	if err := syncDirectory(temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, versionDir); err != nil {
		return fmt.Errorf("dnsname: publish certificate directory %s: %w", versionDir, err)
	}
	return syncDirectory(s.root)
}

func (s *BundleStore) removeOldVersions(current string) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("dnsname: list certificate store %s: %w", s.root, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == current || strings.HasPrefix(entry.Name(), ".certificate-") {
			continue
		}
		if len(entry.Name()) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(entry.Name()); err != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, entry.Name())); err != nil {
			return fmt.Errorf("dnsname: remove old certificate version %s: %w", entry.Name(), err)
		}
	}
	return syncDirectory(s.root)
}

// ValidateBundle checks size, PEM structure, SAN, validity, and key match.
func ValidateBundle(certificatePEM, privateKeyPEM []byte, expectedName string, now time.Time) (Bundle, error) {
	if len(certificatePEM) == 0 || len(certificatePEM) > maximumCertificatePEM {
		return Bundle{}, fmt.Errorf("dnsname: certificate PEM size %d is outside 1..%d", len(certificatePEM), maximumCertificatePEM)
	}
	if len(privateKeyPEM) == 0 || len(privateKeyPEM) > maximumPrivateKeyPEM {
		return Bundle{}, fmt.Errorf("dnsname: private key PEM size %d is outside 1..%d", len(privateKeyPEM), maximumPrivateKeyPEM)
	}
	probe, err := certificateProbeName(expectedName)
	if err != nil {
		return Bundle{}, err
	}
	tlsCertificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return Bundle{}, fmt.Errorf("dnsname: parse certificate and private key: %w", err)
	}
	if len(tlsCertificate.Certificate) == 0 {
		return Bundle{}, errors.New("dnsname: certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(tlsCertificate.Certificate[0])
	if err != nil {
		return Bundle{}, fmt.Errorf("dnsname: parse leaf certificate: %w", err)
	}
	if !slices.Contains(leaf.DNSNames, expectedName) {
		return Bundle{}, fmt.Errorf("dnsname: certificate SANs %q do not contain %s", leaf.DNSNames, expectedName)
	}
	if err := leaf.VerifyHostname(probe); err != nil {
		return Bundle{}, fmt.Errorf("dnsname: certificate does not cover %s: %w", probe, err)
	}
	if now.Before(leaf.NotBefore.Add(-5 * time.Minute)) {
		return Bundle{}, fmt.Errorf("%w before %s", ErrCertificateNotYetValid, leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if !now.Before(leaf.NotAfter) {
		return Bundle{}, fmt.Errorf("%w at %s", ErrCertificateExpired, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	for rest := certificatePEM; len(bytes.TrimSpace(rest)) > 0; {
		block, remainder := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return Bundle{}, errors.New("dnsname: certificate chain contains non-certificate PEM data")
		}
		rest = remainder
	}
	tlsCertificate.Leaf = leaf
	fingerprint := sha256.Sum256(leaf.Raw)
	return Bundle{
		CertificatePEM: bytes.Clone(certificatePEM), PrivateKeyPEM: bytes.Clone(privateKeyPEM),
		Fingerprint: hex.EncodeToString(fingerprint[:]), NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(),
		TLSCertificate: tlsCertificate,
	}, nil
}

// CertificateSource loads and hot-swaps the certificate used by a TLS server.
type CertificateSource struct {
	store   *BundleStore
	current atomic.Pointer[tls.Certificate]
}

// NewCertificateSource loads an existing bundle if one is present.
func NewCertificateSource(store *BundleStore) (*CertificateSource, error) {
	if store == nil {
		return nil, errors.New("dnsname: nil certificate store")
	}
	source := &CertificateSource{store: store}
	bundle, err := store.Load()
	if err == nil {
		source.publish(bundle)
	} else if !errors.Is(err, ErrNoCertificate) && !errors.Is(err, ErrCertificateExpired) && !errors.Is(err, ErrCertificateNotYetValid) {
		return nil, err
	}
	return source, nil
}

// Install publishes a validated bundle and makes new handshakes use it.
func (s *CertificateSource) Install(certificatePEM, privateKeyPEM []byte) (Bundle, error) {
	bundle, err := s.store.Install(certificatePEM, privateKeyPEM)
	if err != nil {
		return Bundle{}, err
	}
	s.publish(bundle)
	return bundle, nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (s *CertificateSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certificate := s.current.Load()
	if certificate == nil {
		return nil, ErrNoCertificate
	}
	return certificate, nil
}

func (s *CertificateSource) publish(bundle Bundle) {
	certificate := bundle.TLSCertificate
	s.current.Store(&certificate)
}

func cloneBundle(bundle Bundle) Bundle {
	bundle.CertificatePEM = bytes.Clone(bundle.CertificatePEM)
	bundle.PrivateKeyPEM = bytes.Clone(bundle.PrivateKeyPEM)
	bundle.TLSCertificate.Certificate = slices.Clone(bundle.TLSCertificate.Certificate)
	return bundle
}

func certificateProbeName(expectedName string) (string, error) {
	if strings.HasPrefix(expectedName, "*.") {
		base, err := canonicalDNSName(strings.TrimPrefix(expectedName, "*."))
		if err != nil {
			return "", err
		}
		return "probe." + base, nil
	}
	return canonicalDNSName(expectedName)
}

func loadOrCreateECDSAKey(path string, random io.Reader) (*ecdsa.PrivateKey, error) {
	contents, err := readSecureFile(path, maximumPrivateKeyPEM)
	if err == nil {
		return parseECDSAKey(contents, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if random == nil {
		random = rand.Reader
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return nil, fmt.Errorf("dnsname: generate ECDSA key: %w", err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("dnsname: encode ECDSA key: %w", err)
	}
	contents = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	if err := publishExclusiveFile(path, contents, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			winner, readErr := readSecureFile(path, maximumPrivateKeyPEM)
			if readErr != nil {
				return nil, readErr
			}
			return parseECDSAKey(winner, path)
		}
		return nil, err
	}
	return key, nil
}

func parseECDSAKey(contents []byte, path string) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("dnsname: parse key %s: invalid PKCS#8 PEM", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("dnsname: parse key %s: %w", path, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("dnsname: key %s is not ECDSA P-256", path)
	}
	return key, nil
}

func marshalPrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("dnsname: encode certificate private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), nil
}

func readSecureFile(path string, maximum int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // read result is authoritative
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("dnsname: inspect open secure file %s: %w", path, err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("dnsname: inspect secure file path %s: %w", path, err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() || !os.SameFile(fileInfo, pathInfo) {
		return nil, fmt.Errorf("dnsname: secure file %s is not regular", path)
	}
	if permissions := fileInfo.Mode().Perm(); permissions != 0o600 {
		return nil, fmt.Errorf("dnsname: secure file %s has permissions %04o, want 0600", path, permissions)
	}
	if fileInfo.Size() <= 0 || fileInfo.Size() > maximum {
		return nil, fmt.Errorf("dnsname: secure file %s size %d is outside 1..%d", path, fileInfo.Size(), maximum)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("dnsname: read secure file %s: %w", path, err)
	}
	if len(contents) == 0 || int64(len(contents)) > maximum {
		return nil, fmt.Errorf("dnsname: secure file %s read size %d is outside 1..%d", path, len(contents), maximum)
	}
	return contents, nil
}

func publishExclusiveFile(path string, contents []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("dnsname: create secure directory %s: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".key-*")
	if err != nil {
		return fmt.Errorf("dnsname: create temporary key: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("dnsname: secure temporary key: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return fmt.Errorf("dnsname: write temporary key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("dnsname: sync temporary key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("dnsname: close temporary key: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func writeAtomicFile(path string, contents []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".atomic-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func writeNewFile(path string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("dnsname: create secure file %s: %w", path, err)
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return fmt.Errorf("dnsname: write secure file %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("dnsname: sync secure file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("dnsname: close secure file %s: %w", path, err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("dnsname: open directory %s for sync: %w", path, err)
	}
	defer directory.Close() //nolint:errcheck // sync result is authoritative
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("dnsname: sync directory %s: %w", path, err)
	}
	return nil
}
