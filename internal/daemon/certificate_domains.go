package daemon

import (
	"crypto/tls"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/domainpolicy"
)

type certificateDomain struct {
	name        string
	installer   certificateInstaller
	tls         *tls.Config
	privateName func() string
	ready       func()
}

type certificateDomains struct {
	targetID  string
	signerID  string
	installMu sync.Mutex
	profile   dnsname.CertificateProfile
	slots     []certificateDomain
}

func configureCertificateDomains(root string, profile dnsname.CertificateProfile, target, signer string) (certificateInstaller, *tls.Config, func() string, func(), error) {
	set := &certificateDomains{profile: profile, targetID: target, signerID: signer}
	policy := domainpolicy.Current()
	for _, domain := range domainpolicy.Domains() {
		slotRoot := filepath.Join(root, "domains", domain)
		if domain == policy.LegacyCertificateDomain {
			slotRoot = root
		}
		name := domainpolicy.Wildcard(domain, profile == dnsname.ProfilePrivateOrigin)
		installer, tlsConfig, privateName, ready, err := configureCertificateProfile(slotRoot, name, profile, target, signer)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		set.slots = append(set.slots, certificateDomain{name: name, installer: installer, tls: tlsConfig, privateName: privateName, ready: ready})
	}
	if len(set.slots) == 0 {
		return nil, nil, nil, nil, errors.New("daemon: HTTPS requires a configured deployment domain")
	}
	current := func() string {
		names := set.privateNames()
		if len(names) == 0 {
			return ""
		}
		return names[0]
	}
	ready := func() {
		for _, slot := range set.slots {
			if slot.ready != nil {
				slot.ready()
			}
		}
	}
	return set, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: set.getCertificate}, current, ready, nil
}

func (s *certificateDomains) privateNames() []string {
	var names []string
	for _, slot := range s.slots {
		if slot.privateName == nil {
			continue
		}
		if name := slot.privateName(); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (s *certificateDomains) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil || hello.ServerName == "" {
		return domainCertificate(s.slots[0], hello)
	}
	private := s.profile == dnsname.ProfilePrivateOrigin
	_, domain, accepted := domainpolicy.Label(strings.ToLower(strings.TrimSuffix(hello.ServerName, ".")), private)
	if !accepted {
		return nil, errors.New("daemon: unconfigured certificate SNI")
	}
	name := domainpolicy.Wildcard(domain, private)
	for _, slot := range s.slots {
		if slot.name == name {
			return domainCertificate(slot, hello)
		}
	}
	return nil, dnsname.ErrNoCertificate
}

func (s *certificateDomains) Install(signed dnsname.SignedBundle) (dnsname.Bundle, bool, error) {
	s.installMu.Lock()
	defer s.installMu.Unlock()
	if signed.Profile != s.profile {
		return dnsname.Bundle{}, false, errors.New("daemon: certificate profile differs from configured slots")
	}
	if _, err := dnsname.VerifySignedBundle(signed, s.targetID, s.signerID, time.Now().UTC()); err != nil {
		return dnsname.Bundle{}, false, fmt.Errorf("daemon: verify domain certificate: %w", err)
	}
	name, err := dnsname.CertificateName(s.profile, signed.CertificatePEM)
	if err != nil {
		return dnsname.Bundle{}, false, fmt.Errorf("daemon: install domain certificate: %w", err)
	}
	if err := s.validatePrivateLabel(signed.PrivateName); err != nil {
		return dnsname.Bundle{}, false, err
	}
	for _, slot := range s.slots {
		if slot.name == name {
			bundle, changed, err := slot.installer.Install(signed)
			if err != nil {
				return dnsname.Bundle{}, false, fmt.Errorf("daemon: install selected certificate: %w", err)
			}
			return bundle, changed, nil
		}
	}
	return dnsname.Bundle{}, false, errors.New("daemon: certificate slot is not configured")
}

func domainCertificate(slot certificateDomain, hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	certificate, err := slot.tls.GetCertificate(hello)
	if err != nil {
		return nil, fmt.Errorf("daemon: select domain certificate: %w", err)
	}
	return certificate, nil
}

func (s *certificateDomains) validatePrivateLabel(name string) error {
	if name == "" {
		return nil
	}
	label, _, _ := domainpolicy.Label(name, true)
	for _, slot := range s.slots {
		pinned, err := slot.installer.(*dnsname.Installer).PinnedPrivateName()
		if err != nil {
			return fmt.Errorf("daemon: read domain pin: %w", err)
		}
		if pinned == "" {
			continue
		}
		existingLabel, _, accepted := domainpolicy.Label(pinned, true)
		if !accepted || existingLabel != label {
			return errors.New("daemon: private label differs from an existing domain pin")
		}
	}
	return nil
}
