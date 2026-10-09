package daemon

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/identity"
)

func TestPrivateServiceCertificateIsPinnedAndRejectsRemovedProfile(t *testing.T) {
	target, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renewer, signer, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := certificateRuntimeConfig{StateDir: t.TempDir(), TargetID: target.ID, OriginHTTPSPort: 8443, OriginRenewerID: renewer.ID}
	runtime, err := configureCertificates(config)
	if err != nil {
		t.Fatal(err)
	}
	if certificateHostReady(runtime.OriginTLS, "fregat.mesh.test", time.Now()) {
		t.Fatal("service host ready without installed certificate")
	}
	installer := runtime.Controller.(*certificateController).installers[dnsname.ProfilePrivateService]
	now := time.Now().UTC()
	certificate, key := daemonTestNamedCertificate(t, 990, now, "*.mesh.test")
	bundle, err := dnsname.ValidateBundle(certificate, key, "*.mesh.test", now)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := dnsname.SignBundle(bundle, target.ID, dnsname.ProfilePrivateService, dnsname.EnvironmentLive, "", signer)
	if err != nil {
		t.Fatal(err)
	}
	removed := signed
	removed.Profile = dnsname.CertificateProfile("public-edge")
	if _, _, err := installer.Install(removed); err == nil {
		t.Fatal("removed profile installed in private service slot")
	}
	if _, _, err := installer.Install(signed); err != nil {
		t.Fatal(err)
	}
	if !certificateHostReady(runtime.OriginTLS, "fregat.mesh.test", now) {
		t.Fatal("installed private service certificate not ready")
	}
	if certificateHostReady(runtime.OriginTLS, "fregat.attacker.invalid", now) || certificateHostReady(runtime.OriginTLS, "fregat.mesh.test", bundle.NotAfter.Add(time.Second)) {
		t.Fatal("invalid or expired private service certificate ready")
	}
	restarted, err := configureCertificates(config)
	if err != nil {
		t.Fatal(err)
	}
	if !certificateHostReady(restarted.OriginTLS, "fregat.mesh.test", now) {
		t.Fatal("restored certificate not ready")
	}
	got, err := restarted.OriginTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: "fregat.mesh.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Leaf.VerifyHostname("fregat.mesh.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.OriginTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: "fregat.attacker.invalid"}); err == nil {
		t.Fatal("unconfigured SNI accepted")
	}
	policy := httpHostPolicy{privateServiceHost: func(host string) bool { return host == "fregat.mesh.test" }}
	if !policy.accepts(httptest.NewRequest("GET", "https://fregat.mesh.test/", nil)) || policy.accepts(httptest.NewRequest("GET", "https://other.mesh.test/", nil)) {
		t.Fatal("private host policy accepted an unregistered name")
	}
}
