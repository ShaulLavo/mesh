package daemon

import (
	"crypto/tls"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/identity"
)

func TestPrivateCertificatesAndPinnedNamesSurviveDomainOverlap(t *testing.T) {
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
	controller := runtime.Controller.(*certificateController)
	now := time.Now().UTC()
	for index, domain := range []string{"mesh.test", "old.test"} {
		wildcard := "*.mesh." + domain
		cert, key := daemonTestNamedCertificate(t, int64(900+index), now, wildcard)
		bundle, err := dnsname.ValidateBundle(cert, key, wildcard, now)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := dnsname.SignBundle(bundle, target.ID, dnsname.ProfilePrivateOrigin, dnsname.EnvironmentLive, "pc.mesh."+domain, signer)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := controller.installers[dnsname.ProfilePrivateOrigin].Install(signed); err != nil {
			t.Fatal(err)
		}
	}
	if len(runtime.PrivateNames()) != 0 {
		t.Fatal("names were published before ingress readiness")
	}
	runtime.PrivateNameReady()
	if len(runtime.PrivateNames()) != 2 {
		t.Fatalf("pinned names: %v", runtime.PrivateNames())
	}
	restarted, err := configureCertificates(config)
	if err != nil {
		t.Fatal(err)
	}
	restarted.PrivateNameReady()
	policy := httpHostPolicy{privateName: restarted.PrivateName, privateNames: restarted.PrivateNames}
	for _, domain := range []string{"mesh.test", "old.test"} {
		host := "pc.mesh." + domain
		certificate, err := restarted.OriginTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: host})
		if err != nil {
			t.Fatal(err)
		}
		if err := certificate.Leaf.VerifyHostname(host); err != nil {
			t.Fatal(err)
		}
		if _, err := restarted.OriginTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: strings.ToUpper(host) + "."}); err != nil {
			t.Fatalf("canonical SNI alias: %v", err)
		}
		if !policy.accepts(httptest.NewRequest("GET", "https://"+host+"/platform", nil)) {
			t.Fatalf("installed name rejected: %s", host)
		}
	}
	if _, err := restarted.OriginTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: "pc.mesh.other.test"}); err == nil {
		t.Fatal("unconfigured SNI accepted")
	}
	if policy.accepts(httptest.NewRequest("GET", "https://other.mesh.old.test/platform", nil)) {
		t.Fatal("unpinned private label accepted")
	}
}

func TestPublicCertificateDomainsKeepStagingSeparate(t *testing.T) {
	target, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renewer, signer, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := certificateRuntimeConfig{StateDir: t.TempDir(), TargetID: target.ID, PublicMode: edge.ModeDirectTLS, PublicCertificatePin: renewer.ID}
	runtime, err := configureCertificates(config)
	if err != nil {
		t.Fatal(err)
	}
	installer := runtime.Controller.(*certificateController).installers[dnsname.ProfilePublicEdge]
	now := time.Now().UTC()
	for index, domain := range []string{"mesh.test", "old.test"} {
		wildcard := "*." + domain
		cert, key := daemonTestNamedCertificate(t, int64(920+index), now, wildcard)
		bundle, err := dnsname.ValidateBundle(cert, key, wildcard, now)
		if err != nil {
			t.Fatal(err)
		}
		staged, err := dnsname.SignBundle(bundle, target.ID, dnsname.ProfilePublicEdge, dnsname.EnvironmentStaging, "", signer)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := installer.Install(staged); err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.PublicTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: "apps." + domain}); err == nil {
			t.Fatal("staging reached live TLS")
		}
		live, err := dnsname.SignBundle(bundle, target.ID, dnsname.ProfilePublicEdge, dnsname.EnvironmentLive, "", signer)
		if err != nil {
			t.Fatal(err)
		}
		bad := live
		bad.Signature = append([]byte(nil), live.Signature...)
		bad.Signature[0] ^= 1
		if _, _, err := installer.Install(bad); err == nil {
			t.Fatal("invalid signature published")
		}
		if _, _, err := installer.Install(live); err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := configureCertificates(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"mesh.test", "old.test"} {
		cert, err := restarted.PublicTLS.GetCertificate(&tls.ClientHelloInfo{ServerName: "7k3d." + domain})
		if err != nil {
			t.Fatal(err)
		}
		if err := cert.Leaf.VerifyHostname("7k3d." + domain); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentPrivateDomainsCannotPinDifferentLabels(t *testing.T) {
	target, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renewer, signer, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := configureCertificates(certificateRuntimeConfig{StateDir: t.TempDir(), TargetID: target.ID, OriginHTTPSPort: 8443, OriginRenewerID: renewer.ID})
	if err != nil {
		t.Fatal(err)
	}
	installer := runtime.Controller.(*certificateController).installers[dnsname.ProfilePrivateOrigin]
	now := time.Now().UTC()
	var signed []dnsname.SignedBundle
	for index, domain := range []string{"mesh.test", "old.test"} {
		wildcard := "*.mesh." + domain
		cert, key := daemonTestNamedCertificate(t, int64(930+index), now, wildcard)
		bundle, err := dnsname.ValidateBundle(cert, key, wildcard, now)
		if err != nil {
			t.Fatal(err)
		}
		label := []string{"pc", "other"}[index]
		value, err := dnsname.SignBundle(bundle, target.ID, dnsname.ProfilePrivateOrigin, dnsname.EnvironmentLive, label+".mesh."+domain, signer)
		if err != nil {
			t.Fatal(err)
		}
		signed = append(signed, value)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, value := range signed {
		group.Add(1)
		go func() { defer group.Done(); _, _, err := installer.Install(value); results <- err }()
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("different labels installed: %d successes", successes)
	}
	runtime.PrivateNameReady()
	if len(runtime.PrivateNames()) != 1 {
		t.Fatalf("published conflicting names: %v", runtime.PrivateNames())
	}
}
