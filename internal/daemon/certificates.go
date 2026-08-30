package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"path/filepath"
	"strings"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/protocol"
)

const privateTLSDirectoryName = "private-tls"

type certificateInstaller interface {
	Install(dnsname.SignedBundle) (dnsname.Bundle, bool, error)
}

type certificateController struct {
	installer certificateInstaller
}

func newCertificateController(installer certificateInstaller) (*certificateController, error) {
	if installer == nil {
		return nil, errors.New("daemon: nil certificate installer")
	}
	return &certificateController{installer: installer}, nil
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
	installed, _, err := c.installer.Install(dnsname.SignedBundle{
		Environment:    dnsname.RenewalEnvironment(request.Certificate.Environment),
		TargetID:       request.Certificate.TargetID,
		SignerID:       request.Certificate.SignerID,
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
	}, true, nil
}

type disabledCertificateController struct{}

func (disabledCertificateController) HandleControl(context.Context, protocol.Control) (protocol.Control, bool, error) {
	return protocol.Control{}, false, nil
}

func configureOriginCertificates(stateDir, targetID, signerID string, httpsPort uint16) (controlHandler, *tls.Config, error) {
	if httpsPort == 0 {
		if strings.TrimSpace(signerID) != "" {
			return nil, nil, errors.New("daemon: certificate renewer ID requires a non-zero HTTPS port")
		}
		return disabledCertificateController{}, nil, nil
	}
	if strings.TrimSpace(signerID) == "" {
		return nil, nil, errors.New("daemon: HTTPS port requires a pinned certificate renewer ID")
	}
	liveStore, err := dnsname.NewBundleStore(filepath.Join(stateDir, privateTLSDirectoryName, string(dnsname.EnvironmentLive)), dnsname.WildcardName)
	if err != nil {
		return nil, nil, err
	}
	stagingStore, err := dnsname.NewBundleStore(filepath.Join(stateDir, privateTLSDirectoryName, string(dnsname.EnvironmentStaging)), dnsname.WildcardName)
	if err != nil {
		return nil, nil, err
	}
	source, err := dnsname.NewCertificateSource(liveStore)
	if err != nil {
		return nil, nil, err
	}
	installer, err := dnsname.NewInstaller(dnsname.InstallerConfig{
		LiveSource: source, StagingStore: stagingStore, TargetID: targetID, SignerID: signerID, ExpectedName: dnsname.WildcardName,
	})
	if err != nil {
		return nil, nil, err
	}
	controller, err := newCertificateController(installer)
	if err != nil {
		return nil, nil, err
	}
	return controller, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: source.GetCertificate}, nil
}
