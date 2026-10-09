package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

func TestCertificateControllerInstallsAndAcknowledgesBundle(t *testing.T) {
	want := dnsname.SignedBundle{
		Profile:     dnsname.ProfilePrivateOrigin,
		Environment: dnsname.EnvironmentStaging,
		TargetID:    "origin", SignerID: "renewer", PrivateName: "pc.mesh.mesh.test", CertificatePEM: []byte("certificate"),
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
	if response.Type != protocol.TypeCertificateInstalled || response.RequestID != "certificate-1" || response.CertificateFingerprint != "fingerprint" || response.CertificateEnvironment != "staging" || response.CertificateProfile != "private-origin" || response.CertificatePrivateName != "pc.mesh.mesh.test" {
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
	server, err := newClientServer(lifecycle, failingServerTestConnector(), noServiceControl{}, controller)
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

func TestConfigureCertificatesWithoutHTTPSCreatesNoCertificates(t *testing.T) {
	stateDir := t.TempDir()
	runtime, err := configureCertificates(certificateRuntimeConfig{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.OriginTLS != nil || runtime.AppRegistryTLS != nil {
		t.Fatalf("disabled TLS runtime = origin %v service %v", runtime.OriginTLS, runtime.AppRegistryTLS)
	}
	if _, err := os.Stat(filepath.Join(stateDir, certificateDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled runtime touched certificate state: %v", err)
	}
}

func TestConfigureCertificatesSharesPrivateServiceSourceWithRegistry(t *testing.T) {
	target, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateRenewer, privateSigner, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	runtime, err := configureCertificates(certificateRuntimeConfig{
		StateDir: stateDir, TargetID: target.ID,
		OriginHTTPSPort: 8443, OriginRenewerID: privateRenewer.ID,
		AppRegistryRenewerID: privateRenewer.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.OriginTLS == nil || runtime.AppRegistryTLS == nil || runtime.OriginTLS.GetCertificate == nil || runtime.AppRegistryTLS.GetCertificate == nil {
		t.Fatal("combined runtime did not construct origin and registry TLS sources")
	}
	controller, ok := runtime.Controller.(*certificateController)
	if !ok {
		t.Fatalf("certificate controller = %T", runtime.Controller)
	}
	now := time.Now().UTC()
	serviceCertificate, serviceKey := daemonTestNamedCertificate(t, 801, now, dnsname.ServiceWildcardName())
	serviceBundle, err := dnsname.ValidateBundle(serviceCertificate, serviceKey, dnsname.ServiceWildcardName(), now)
	if err != nil {
		t.Fatal(err)
	}
	serviceStaging, err := dnsname.SignBundle(serviceBundle, target.ID, dnsname.ProfilePrivateService, dnsname.EnvironmentStaging, "", privateSigner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.installers[dnsname.ProfilePrivateService].Install(serviceStaging); err != nil {
		t.Fatal(err)
	}
	serviceStagingPath := filepath.Join(stateDir, certificateDirectoryName, string(dnsname.ProfilePrivateService), string(dnsname.EnvironmentStaging))
	if info, err := os.Stat(serviceStagingPath); err != nil || !info.IsDir() {
		t.Fatalf("service staging slot %s: %v", serviceStagingPath, err)
	}
	if _, err := runtime.AppRegistryTLS.GetCertificate(nil); !errors.Is(err, dnsname.ErrNoCertificate) {
		t.Fatalf("service staging certificate entered live TLS source: %v", err)
	}
	serviceLive, err := dnsname.SignBundle(serviceBundle, target.ID, dnsname.ProfilePrivateService, dnsname.EnvironmentLive, "", privateSigner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.installers[dnsname.ProfilePrivateService].Install(serviceLive); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.AppRegistryTLS.GetCertificate(nil); err != nil {
		t.Fatalf("service live certificate was not hot-published: %v", err)
	}

	originShort, err := runtime.OriginTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: "fregat.mesh.test"})
	if err != nil {
		t.Fatal(err)
	}
	registryShort, err := runtime.AppRegistryTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: "7k3d.mesh.test"})
	if err != nil {
		t.Fatal(err)
	}
	if originShort != registryShort {
		t.Fatal("origin and registry did not share the private-service TLS source")
	}

	privateCertificate, privateKey := daemonTestNamedCertificate(t, 802, now, dnsname.WildcardName())
	privateBundle, err := dnsname.ValidateBundle(privateCertificate, privateKey, dnsname.WildcardName(), now)
	if err != nil {
		t.Fatal(err)
	}
	privateLive, err := dnsname.SignBundle(privateBundle, target.ID, dnsname.ProfilePrivateOrigin, dnsname.EnvironmentLive, "pc.mesh.mesh.test", privateSigner)
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
	if got := runtime.PrivateName(); got != "pc.mesh.mesh.test" {
		t.Fatalf("private name after ingress readiness = %q", got)
	}
	restarted, err := configureCertificates(certificateRuntimeConfig{
		StateDir: stateDir, TargetID: target.ID,
		OriginHTTPSPort: 8443, OriginRenewerID: privateRenewer.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.PrivateName(); got != "" {
		t.Fatalf("restarted private name was exposed before ingress readiness: %q", got)
	}
	restarted.PrivateNameReady()
	if got := restarted.PrivateName(); got != "pc.mesh.mesh.test" {
		t.Fatalf("restarted private name after ingress readiness = %q", got)
	}
	for _, path := range []string{
		filepath.Join(stateDir, privateTLSDirectoryName, string(dnsname.EnvironmentLive)),
		filepath.Join(stateDir, certificateDirectoryName, string(dnsname.ProfilePrivateService), string(dnsname.EnvironmentLive)),
	} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("certificate slot %s: %v", path, err)
		}
	}
}

func TestConfigureCertificatesRejectsInvalidRegistryAndConflictingPins(t *testing.T) {
	for name, config := range map[string]certificateRuntimeConfig{
		"invalid registry pin":  {StateDir: t.TempDir(), TargetID: "invalid", AppRegistryRenewerID: "invalid"},
		"conflicting signers":   {StateDir: t.TempDir(), OriginHTTPSPort: 8443, OriginRenewerID: "origin", AppRegistryRenewerID: "registry"},
		"origin without HTTPS":  {StateDir: t.TempDir(), OriginRenewerID: "origin"},
		"origin without signer": {StateDir: t.TempDir(), OriginHTTPSPort: 8443},
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
