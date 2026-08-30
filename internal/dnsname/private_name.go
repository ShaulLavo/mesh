package dnsname

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	privateNameFile         = "private-name"
	maximumPrivateNameBytes = 253
)

// PrivateNameSource persists and publishes the private DNS name proven by the
// Pi's latest successful live certificate distribution. Staging installs
// deliberately never reach this source.
type PrivateNameSource struct {
	path      string
	liveStore *BundleStore
	mu        sync.Mutex
	current   atomic.Pointer[string]
	ingress   atomic.Bool
}

// NewPrivateNameSource restores a private name only when the same live slot
// also contains a currently valid private wildcard certificate.
func NewPrivateNameSource(liveStore *BundleStore) (*PrivateNameSource, error) {
	if liveStore == nil {
		return nil, errors.New("dnsname: nil live store for private name")
	}
	source := &PrivateNameSource{path: filepath.Join(liveStore.root, privateNameFile), liveStore: liveStore}
	if err := source.load(); err != nil {
		return nil, err
	}
	return source, nil
}

// Refresh reloads the persisted name after a valid live certificate becomes
// available. This lets an empty later distribution preserve a name that was
// already proven before a restart with an expired certificate.
func (s *PrivateNameSource) Refresh() error {
	if s == nil || s.liveStore == nil {
		return errors.New("dnsname: nil private-name source")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *PrivateNameSource) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *PrivateNameSource) loadLocked() error {
	name, err := s.readPersisted()
	if err != nil {
		return err
	}
	if _, err := s.liveStore.Load(); err != nil {
		if errors.Is(err, ErrNoCertificate) || errors.Is(err, ErrCertificateExpired) || errors.Is(err, ErrCertificateNotYetValid) {
			s.current.Store(nil)
			return nil
		}
		return err
	}
	if name == "" {
		s.current.Store(nil)
		return nil
	}
	s.publish(name)
	return nil
}

func (s *PrivateNameSource) readPersisted() (string, error) {
	contents, err := readSecureFile(s.path, maximumPrivateNameBytes+1)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("dnsname: read persisted private name: %w", err)
	}
	name := strings.TrimSuffix(string(contents), "\n")
	if string(contents) != name+"\n" {
		return "", errors.New("dnsname: persisted private name is not canonical")
	}
	if err := ValidatePrivateName(name); err != nil {
		return "", fmt.Errorf("dnsname: persisted private name: %w", err)
	}
	return name, nil
}

// Current returns the private name restored or installed alongside a valid
// live certificate, once the ingress readiness gate has opened.
func (s *PrivateNameSource) Current() string {
	if s == nil || !s.ingress.Load() {
		return ""
	}
	return s.installed()
}

func (s *PrivateNameSource) installed() string {
	if s == nil {
		return ""
	}
	value := s.current.Load()
	if value == nil {
		return ""
	}
	return *value
}

// MarkIngressReady allows HostInfo to expose the persisted name only after
// Tailscale has successfully published TCP/443 to the origin HTTPS listener.
func (s *PrivateNameSource) MarkIngressReady() {
	if s != nil {
		s.ingress.Store(true)
	}
}

// Install atomically persists and publishes one non-empty canonical private
// name. Callers must first verify and install the corresponding live bundle.
func (s *PrivateNameSource) Install(name string) error {
	if s == nil {
		return errors.New("dnsname: nil private-name source")
	}
	if err := ValidatePrivateName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.readPersisted()
	if err != nil {
		return err
	}
	if existing != "" && existing != name {
		return fmt.Errorf("dnsname: private name is already pinned to %s; reset and re-adopt the origin before renaming it", existing)
	}
	if existing == name {
		s.publish(name)
		return nil
	}
	if err := writeAtomicFile(s.path, []byte(name+"\n"), 0o600); err != nil {
		return fmt.Errorf("dnsname: persist private name: %w", err)
	}
	s.publish(name)
	return nil
}

func (s *PrivateNameSource) publish(name string) {
	copyName := name
	s.current.Store(&copyName)
}

// ValidatePrivateName requires one canonical label below mesh.shaulavo.dev.
func ValidatePrivateName(name string) error {
	suffix := "." + PrivateZone
	if name == "" || len(name) > maximumPrivateNameBytes || strings.TrimSpace(name) != name || !strings.HasSuffix(name, suffix) {
		return errors.New("dnsname: private name must be one canonical label below mesh.shaulavo.dev")
	}
	label := strings.TrimSuffix(name, suffix)
	canonical, err := privateHostName(label)
	if err != nil || canonical != name {
		return errors.New("dnsname: private name must be one canonical label below mesh.shaulavo.dev")
	}
	return nil
}

func validateCertificatePrivateName(profile CertificateProfile, name string) error {
	switch profile {
	case ProfilePrivateOrigin:
		if name == "" {
			return nil
		}
		return ValidatePrivateName(name)
	case ProfilePublicEdge:
		if name != "" {
			return errors.New("dnsname: public-edge certificate must not carry a private name")
		}
		return nil
	default:
		return fmt.Errorf("dnsname: unsupported certificate profile %q", profile)
	}
}
