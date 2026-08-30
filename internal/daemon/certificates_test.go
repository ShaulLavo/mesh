package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/protocol"
)

func TestCertificateControllerInstallsAndAcknowledgesBundle(t *testing.T) {
	want := dnsname.SignedBundle{
		Environment: dnsname.EnvironmentStaging,
		TargetID:    "origin", SignerID: "renewer", CertificatePEM: []byte("certificate"),
		PrivateKeyPEM: []byte("private-key"), Signature: []byte("signature"),
	}
	installer := &certificateInstallerStub{bundle: dnsname.Bundle{Fingerprint: "fingerprint"}}
	controller, err := newCertificateController(installer)
	if err != nil {
		t.Fatal(err)
	}
	response, handled, err := controller.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeCertificateInstall, RequestID: "certificate-1",
		Certificate: &protocol.CertificateInstall{
			Environment: string(want.Environment),
			TargetID:    want.TargetID, SignerID: want.SignerID, CertificatePEM: want.CertificatePEM,
			PrivateKeyPEM: want.PrivateKeyPEM, Signature: want.Signature,
		},
	})
	if err != nil || !handled {
		t.Fatalf("handled = %v, error = %v", handled, err)
	}
	if response.Type != protocol.TypeCertificateInstalled || response.RequestID != "certificate-1" || response.CertificateFingerprint != "fingerprint" || response.CertificateEnvironment != "staging" {
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
	controller, err := newCertificateController(&certificateInstallerStub{err: errors.New("bad signature")})
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]protocol.Control{
		"request ID": {Type: protocol.TypeCertificateInstall, Certificate: &protocol.CertificateInstall{}},
		"bundle":     {Type: protocol.TypeCertificateInstall, RequestID: "certificate-1"},
		"installer":  {Type: protocol.TypeCertificateInstall, RequestID: "certificate-1", Certificate: &protocol.CertificateInstall{}},
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
	controller, err := newCertificateController(installer)
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
		Certificate: &protocol.CertificateInstall{Environment: "live", TargetID: "origin", SignerID: "renewer", Signature: []byte("signed")},
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

type certificateInstallerStub struct {
	got    dnsname.SignedBundle
	bundle dnsname.Bundle
	err    error
}

func (s *certificateInstallerStub) Install(bundle dnsname.SignedBundle) (dnsname.Bundle, bool, error) {
	s.got = bundle
	return s.bundle, s.err == nil, s.err
}
