package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

func TestCertificateControllerInstallsAndAcknowledgesBundle(t *testing.T) {
	want := dnsname.SignedBundle{
		Profile:     dnsname.ProfilePrivateOrigin,
		Environment: dnsname.EnvironmentStaging,
		TargetID:    "origin", SignerID: "renewer", PrivateName: "pc.mesh.shaulavo.dev", CertificatePEM: []byte("certificate"),
		PrivateKeyPEM: []byte("private-key"), Signature: []byte("signature"),
	}
	installer := &certificateInstallerStub{bundle: dnsname.Bundle{Fingerprint: "fingerprint"}}
	controller, err := newCertificateController(map[dnsname.CertificateProfile]certificateInstaller{dnsname.ProfilePrivateOrigin: installer})
	if err != nil {
		t.Fatal(err)
	}
	response, handled, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeCertificateInstall, RequestID: "certificate-1",
		Certificate: &protocol.CertificateInstall{
			Profile:     string(want.Profile),
			Environment: string(want.Environment),
			TargetID:    want.TargetID, SignerID: want.SignerID, PrivateName: want.PrivateName, CertificatePEM: want.CertificatePEM,
			PrivateKeyPEM: want.PrivateKeyPEM, Signature: want.Signature,
		},
	})
	if err != nil || !handled {
		t.Fatalf("handled = %v, error = %v", handled, err)
	}
	if response.Type != protocol.TypeCertificateInstalled || response.RequestID != "certificate-1" || response.CertificateFingerprint != "fingerprint" || response.CertificateEnvironment != "staging" || response.CertificateProfile != "private-origin" || response.CertificatePrivateName != "pc.mesh.shaulavo.dev" {
		t.Fatalf("response = %#v", response)
	}
	if !reflect.DeepEqual(installer.got, want) {
		t.Fatalf("installed = %#v, want %#v", installer.got, want)
	}
	if _, handled, err := controller.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeList}); handled || err != nil {
		t.Fatalf("unrelated request handled = %v, error = %v", handled, err)
	}
}

func TestCertificateControllerRejectsInvalidRequests(t *testing.T) {
	controller, err := newCertificateController(map[dnsname.CertificateProfile]certificateInstaller{dnsname.ProfilePrivateOrigin: &certificateInstallerStub{err: errors.New("bad signature")}})
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]protocol.Control{
		"request ID": {Type: protocol.TypeCertificateInstall, Certificate: &protocol.CertificateInstall{}},
		"bundle":     {Type: protocol.TypeCertificateInstall, RequestID: "certificate-1"},
		"profile":    {Type: protocol.TypeCertificateInstall, RequestID: "certificate-1", Certificate: &protocol.CertificateInstall{}},
		"installer":  {Type: protocol.TypeCertificateInstall, RequestID: "certificate-1", Certificate: &protocol.CertificateInstall{Profile: "private-origin"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, handled, err := controller.HandleControl(context.Background(), request); !handled || err == nil {
				t.Fatalf("handled = %v, error = %v", handled, err)
			}
		})
	}
}

func TestClientServerDispatchesCertificateInstall(t *testing.T) {
	installer := &certificateInstallerStub{bundle: dnsname.Bundle{Fingerprint: "installed-fingerprint"}}
	controller, err := newCertificateController(map[dnsname.CertificateProfile]certificateInstaller{dnsname.ProfilePrivateOrigin: installer})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, noServiceControl{}, controller)
	if err != nil {
		t.Fatal(err)
	}
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(context.Background(), client) }()
	client.pushRead(serverControlFrame(t, protocol.Control{
		Type: protocol.TypeCertificateInstall, RequestID: "certificate-through-server",
		Certificate: &protocol.CertificateInstall{Profile: "private-origin", Environment: "live", TargetID: "origin", SignerID: "renewer", Signature: []byte("signed")},
	}))
	response := decodeServerControl(t, client.nextWrite(t))
	if response.Type != protocol.TypeCertificateInstalled || response.RequestID != "certificate-through-server" || response.CertificateFingerprint != "installed-fingerprint" {
		t.Fatalf("response = %#v", response)
	}
	client.pushReadError(context.Canceled)
	if err := waitServerResult(t, done, "certificate request server"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureCertificatesKeepsProxyModeCertificateFree(t *testing.T) {
	stateDir := t.TempDir()
	runtime, err := configureCertificates(certificateRuntimeConfig{StateDir: stateDir, PublicMode: edge.ModeProxy})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.OriginTLS != nil || runtime.PublicTLS != nil {
		t.Fatalf("proxy TLS runtime = origin %v public %v", runtime.OriginTLS, runtime.PublicTLS)
	}
	if _, err := os.Stat(filepath.Join(stateDir, certificateDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proxy mode touched public certificate state: %v", err)
	}
}

func TestConfigureCertificatesSeparatesPrivateAndPublicProfiles(t *testing.T) {
	target, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateRenewer, privateSigner, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publicRenewer, publicSigner, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	runtime, err := configureCertificates(certificateRuntimeConfig{
		StateDir: stateDir, TargetID: target.ID,
		OriginHTTPSPort: 8443, OriginRenewerID: privateRenewer.ID,
		PublicMode: edge.ModeDirectTLS, PublicCertificatePin: publicRenewer.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.OriginTLS == nil || runtime.PublicTLS == nil || runtime.OriginTLS.GetCertificate == nil || runtime.PublicTLS.GetCertificate == nil {
		t.Fatal("combined runtime did not construct both independent TLS sources")
	}
	controller, ok := runtime.Controller.(*certificateController)
	if !ok {
		t.Fatalf("certificate controller = %T", runtime.Controller)
	}
	now := time.Now().UTC()
	publicCertificate, publicKey := daemonTestNamedCertificate(t, 801, now, dnsname.PublicWildcardName)
	publicBundle, err := dnsname.ValidateBundle(publicCertificate, publicKey, dnsname.PublicWildcardName, now)
	if err != nil {
		t.Fatal(err)
	}
	publicStaging, err := dnsname.SignBundle(publicBundle, target.ID, dnsname.ProfilePublicEdge, dnsname.EnvironmentStaging, "", publicSigner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.installers[dnsname.ProfilePublicEdge].Install(publicStaging); err != nil {
		t.Fatal(err)
	}
	publicStagingPath := filepath.Join(stateDir, certificateDirectoryName, string(dnsname.ProfilePublicEdge), string(dnsname.EnvironmentStaging))
	if info, err := os.Stat(publicStagingPath); err != nil || !info.IsDir() {
		t.Fatalf("public staging slot %s: %v", publicStagingPath, err)
	}
	if _, err := runtime.PublicTLS.GetCertificate(nil); !errors.Is(err, dnsname.ErrNoCertificate) {
		t.Fatalf("public staging certificate entered live TLS source: %v", err)
	}
	publicLive, err := dnsname.SignBundle(publicBundle, target.ID, dnsname.ProfilePublicEdge, dnsname.EnvironmentLive, "", publicSigner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.installers[dnsname.ProfilePublicEdge].Install(publicLive); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.PublicTLS.GetCertificate(nil); err != nil {
		t.Fatalf("public live certificate was not hot-published: %v", err)
	}

	privateCertificate, privateKey := daemonTestNamedCertificate(t, 802, now, dnsname.WildcardName)
	privateBundle, err := dnsname.ValidateBundle(privateCertificate, privateKey, dnsname.WildcardName, now)
	if err != nil {
		t.Fatal(err)
	}
	privateLive, err := dnsname.SignBundle(privateBundle, target.ID, dnsname.ProfilePrivateOrigin, dnsname.EnvironmentLive, "pc.mesh.shaulavo.dev", privateSigner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.installers[dnsname.ProfilePrivateOrigin].Install(privateLive); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.OriginTLS.GetCertificate(nil); err != nil {
		t.Fatalf("private live certificate was not hot-published: %v", err)
	}
	if got := runtime.PrivateName(); got != "" {
		t.Fatalf("private name was exposed before ingress readiness: %q", got)
	}
	runtime.PrivateNameReady()
	if got := runtime.PrivateName(); got != "pc.mesh.shaulavo.dev" {
		t.Fatalf("private name after ingress readiness = %q", got)
	}
	restarted, err := configureCertificates(certificateRuntimeConfig{
		StateDir: stateDir, TargetID: target.ID,
		OriginHTTPSPort: 8443, OriginRenewerID: privateRenewer.ID,
		PublicMode: edge.ModeProxy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.PrivateName(); got != "" {
		t.Fatalf("restarted private name was exposed before ingress readiness: %q", got)
	}
	restarted.PrivateNameReady()
	if got := restarted.PrivateName(); got != "pc.mesh.shaulavo.dev" {
		t.Fatalf("restarted private name after ingress readiness = %q", got)
	}
	for _, path := range []string{
		filepath.Join(stateDir, privateTLSDirectoryName, string(dnsname.EnvironmentLive)),
		filepath.Join(stateDir, certificateDirectoryName, string(dnsname.ProfilePublicEdge), string(dnsname.EnvironmentLive)),
	} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("certificate slot %s: %v", path, err)
		}
	}
}

func TestConfigureCertificatesRejectsIncompleteDirectTLSProfile(t *testing.T) {
	for name, config := range map[string]certificateRuntimeConfig{
		"missing pin": {StateDir: t.TempDir(), PublicMode: edge.ModeDirectTLS},
		"invalid pin": {StateDir: t.TempDir(), TargetID: "invalid", PublicMode: edge.ModeDirectTLS, PublicCertificatePin: "invalid"},
		"proxy pin":   {StateDir: t.TempDir(), PublicMode: edge.ModeProxy, PublicCertificatePin: "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := configureCertificates(config); err == nil {
				t.Fatal("invalid certificate profile configured")
			}
		})
	}
}

type certificateInstallerStub struct {
	got    dnsname.SignedBundle
	bundle dnsname.Bundle
	err    error
}

func (s *certificateInstallerStub) Install(bundle dnsname.SignedBundle) (dnsname.Bundle, bool, error) {
	s.got = bundle
	return s.bundle, s.err == nil, s.err
}
