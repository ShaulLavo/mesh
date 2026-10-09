package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/protocol"
)

const (
	privateTLSDirectoryName  = "private-tls"
	certificateDirectoryName = "certificates"
)

type certificateInstaller interface {
	Install(dnsname.SignedBundle) (dnsname.Bundle, bool, error)
}

type certificateController struct {
	installers map[dnsname.CertificateProfile]certificateInstaller
}

func newCertificateController(installers map[dnsname.CertificateProfile]certificateInstaller) (*certificateController, error) {
	if len(installers) == 0 {
		return nil, errors.New("daemon: no certificate installers")
	}
	copyInstallers := make(map[dnsname.CertificateProfile]certificateInstaller, len(installers))
	for profile, installer := range installers {
		if installer == nil {
			return nil, errors.New("daemon: nil certificate installer")
		}
		if profile != dnsname.ProfilePrivateOrigin && profile != dnsname.ProfilePrivateService {
			return nil, errors.New("daemon: unsupported certificate installer profile")
		}
		copyInstallers[profile] = installer
	}
	return &certificateController{installers: copyInstallers}, nil
}

func (c *certificateController) HandleControl(_ context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type != protocol.TypeCertificateInstall {
		return protocol.Control{}, false, nil
	}
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, true, err
	}
	if request.Certificate == nil {
		return protocol.Control{}, true, errors.New("daemon: certificate.install requires a certificate bundle")
	}
	profile := dnsname.CertificateProfile(request.Certificate.Profile)
	installer, ok := c.installers[profile]
	if !ok {
		return protocol.Control{}, true, errors.New("daemon: certificate.install profile is not configured")
	}
	installed, _, err := installer.Install(dnsname.SignedBundle{
		Profile:        profile,
		Environment:    dnsname.RenewalEnvironment(request.Certificate.Environment),
		TargetID:       request.Certificate.TargetID,
		SignerID:       request.Certificate.SignerID,
		PrivateName:    request.Certificate.PrivateName,
		CertificatePEM: request.Certificate.CertificatePEM,
		PrivateKeyPEM:  request.Certificate.PrivateKeyPEM,
		Signature:      request.Certificate.Signature,
	})
	if err != nil {
		return protocol.Control{}, true, err
	}
	return protocol.Control{
		Type: protocol.TypeCertificateInstalled, RequestID: request.RequestID,
		CertificateFingerprint: installed.Fingerprint, CertificateEnvironment: request.Certificate.Environment,
		CertificateProfile:     request.Certificate.Profile,
		CertificatePrivateName: request.Certificate.PrivateName,
	}, true, nil
}

type disabledCertificateController struct{}

func (disabledCertificateController) HandleControl(context.Context, protocol.Control) (protocol.Control, bool, error) {
	return protocol.Control{}, false, nil
}

type certificateRuntimeConfig struct {
	StateDir             string
	TargetID             string
	OriginHTTPSPort      uint16
	OriginRenewerID      string
	AppRegistryRenewerID string
}

type certificateRuntime struct {
	Controller       controlHandler
	OriginTLS        *tls.Config
	AppRegistryTLS   *tls.Config
	PrivateName      func() string
	PrivateNames     func() []string
	PrivateNameReady func()
}

func configureCertificates(config certificateRuntimeConfig) (certificateRuntime, error) {
	if config.OriginHTTPSPort == 0 && strings.TrimSpace(config.OriginRenewerID) != "" {
		return certificateRuntime{}, errors.New("daemon: certificate renewer ID requires a non-zero HTTPS port")
	}
	if config.OriginHTTPSPort != 0 && strings.TrimSpace(config.OriginRenewerID) == "" {
		return certificateRuntime{}, errors.New("daemon: HTTPS port requires a pinned certificate renewer ID")
	}
	if config.OriginHTTPSPort != 0 && config.AppRegistryRenewerID != "" && config.OriginRenewerID != config.AppRegistryRenewerID {
		return certificateRuntime{}, errors.New("daemon: origin and app registry require the same private-service certificate renewer ID")
	}

	installers := make(map[dnsname.CertificateProfile]certificateInstaller, 2)
	runtime := certificateRuntime{}
	if config.OriginHTTPSPort != 0 {
		installer, tlsConfig, privateName, privateNameReady, err := configureCertificateDomains(
			filepath.Join(config.StateDir, privateTLSDirectoryName),
			dnsname.ProfilePrivateOrigin, config.TargetID, config.OriginRenewerID,
		)
		if err != nil {
			return certificateRuntime{}, err
		}
		installers[dnsname.ProfilePrivateOrigin] = installer
		runtime.OriginTLS = tlsConfig
		runtime.PrivateName = privateName
		runtime.PrivateNames = installer.(*certificateDomains).privateNames
		runtime.PrivateNameReady = privateNameReady
	}
	serviceRenewerID := config.OriginRenewerID
	if config.AppRegistryRenewerID != "" {
		serviceRenewerID = config.AppRegistryRenewerID
	}
	if serviceRenewerID != "" {
		installer, tlsConfig, _, _, err := configureCertificateDomains(
			filepath.Join(config.StateDir, certificateDirectoryName, string(dnsname.ProfilePrivateService)),
			dnsname.ProfilePrivateService, config.TargetID, serviceRenewerID,
		)
		if err != nil {
			return certificateRuntime{}, err
		}
		installers[dnsname.ProfilePrivateService] = installer
		if runtime.OriginTLS != nil {
			runtime.OriginTLS = privateServiceTLS(runtime.OriginTLS, tlsConfig)
		}
		if config.AppRegistryRenewerID != "" {
			runtime.AppRegistryTLS = tlsConfig
		}
	}

	if len(installers) == 0 {
		runtime.Controller = disabledCertificateController{}
		return runtime, nil
	}
	controller, err := newCertificateController(installers)
	if err != nil {
		return certificateRuntime{}, err
	}
	runtime.Controller = controller
	return runtime, nil
}

func configureCertificateProfile(root, name string, profile dnsname.CertificateProfile, targetID, signerID string) (certificateInstaller, *tls.Config, func() string, func(), error) {
	liveStore, err := dnsname.NewBundleStore(filepath.Join(root, string(dnsname.EnvironmentLive)), name)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stagingStore, err := dnsname.NewBundleStore(filepath.Join(root, string(dnsname.EnvironmentStaging)), name)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	source, err := dnsname.NewCertificateSource(liveStore)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var privateName *dnsname.PrivateNameSource
	if profile == dnsname.ProfilePrivateOrigin {
		privateName, err = dnsname.NewPrivateNameSource(liveStore)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	installer, err := dnsname.NewInstaller(dnsname.InstallerConfig{
		Profile: profile, LiveSource: source, StagingStore: stagingStore, PrivateName: privateName,
		TargetID: targetID, SignerID: signerID,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var currentPrivateName func() string
	var markPrivateNameReady func()
	if privateName != nil {
		currentPrivateName = privateName.Current
		markPrivateNameReady = privateName.MarkIngressReady
	}
	return installer, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: source.GetCertificate}, currentPrivateName, markPrivateNameReady, nil
}

func (c certificateRuntime) viewHostReady(host string, now time.Time) bool {
	return certificateHostReady(c.AppRegistryTLS, host, now)
}

func certificateHostReady(config *tls.Config, host string, now time.Time) bool {
	if _, _, accepted := domainpolicy.Label(host, false); !accepted || config == nil {
		return false
	}
	certificate, err := config.GetCertificate(&tls.ClientHelloInfo{ServerName: host})
	if err != nil || certificate == nil || certificate.Leaf == nil {
		return false
	}
	leaf := certificate.Leaf
	return !now.Before(leaf.NotBefore) && now.Before(leaf.NotAfter) && leaf.VerifyHostname(host) == nil
}
