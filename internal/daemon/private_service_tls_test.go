package daemon

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/identity"
)

func TestPrivateServiceCertificateIsPinnedAndSeparateFromPublicEdge(t *testing.T) {
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
	installer := runtime.Controller.(*certificateController).installers[dnsname.ProfilePrivateService]
	now := time.Now().UTC()
	certificate, key := daemonTestNamedCertificate(t, 990, now, "*.mesh.test")
	bundle, err := dnsname.ValidateBundle(certificate, key, "*.mesh.test", now)
	if err != nil {
		t.Fatal(err)
	}
	public, err := dnsname.SignBundle(bundle, target.ID, dnsname.ProfilePublicEdge, dnsname.EnvironmentLive, "", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := installer.Install(public); err == nil {
		t.Fatal("public edge profile installed in private service slot")
	}
	signed, err := dnsname.SignBundle(bundle, target.ID, dnsname.ProfilePrivateService, dnsname.EnvironmentLive, "", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := installer.Install(signed); err != nil {
		t.Fatal(err)
	}
	restarted, err := configureCertificates(config)
	if err != nil {
		t.Fatal(err)
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
