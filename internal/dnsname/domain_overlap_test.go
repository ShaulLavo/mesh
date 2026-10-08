package dnsname

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestPrivateRecordsUseExplicitAcceptedDomain(t *testing.T) {
	provider := &memoryProvider{}
	for _, domain := range []string{"mesh.test", "old.test"} {
		record, err := ReconcileHostA(context.Background(), provider, HostAddress{Name: "pc", Domain: domain, Address: netip.MustParseAddr("100.88.7.9")})
		if err != nil {
			t.Fatal(err)
		}
		if record.Name != "pc.mesh."+domain {
			t.Fatalf("record %s", record.Name)
		}
	}
	if _, err := ReconcileHostA(context.Background(), provider, HostAddress{Name: "pc", Domain: "other.test", Address: netip.MustParseAddr("100.88.7.9")}); err == nil {
		t.Fatal("unconfigured domain accepted")
	}
}

func TestPrivateNameCannotCrossCertificateSlot(t *testing.T) {
	root := t.TempDir()
	store, err := NewBundleStore(root, "*.mesh.old.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert, key := testCertificate(t, 991, "*.mesh.old.test", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := store.Install(cert, key); err != nil {
		t.Fatal(err)
	}
	source, err := NewPrivateNameSource(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Install("pc.mesh.mesh.test"); err == nil {
		t.Fatal("cross-domain name installed into old slot")
	}
	if err := source.Install("pc.mesh.old.test"); err != nil {
		t.Fatal(err)
	}
	if err := source.Install("other.mesh.old.test"); err == nil {
		t.Fatal("pinned label renamed")
	}
}

func TestCertificateSlotRejectsUnconfiguredOrAmbiguousSANs(t *testing.T) {
	now := time.Now()
	for _, names := range [][]string{{"*.other.test"}, {"*.mesh.test", "*.old.test"}} {
		certificate, _ := testCertificate(t, 992, names[0], now.Add(-time.Hour), now.Add(time.Hour), names[1:]...)
		if _, err := CertificateName(ProfilePublicEdge, certificate); err == nil {
			t.Fatalf("accepted SANs: %v", names)
		}
	}
	certificate, _ := testCertificate(t, 993, "*.old.test", now.Add(-time.Hour), now.Add(time.Hour))
	if name, err := CertificateName(ProfilePublicEdge, certificate); err != nil || name != "*.old.test" {
		t.Fatalf("alias certificate slot: %s %v", name, err)
	}
	if _, err := CertificateName(ProfilePrivateOrigin, certificate); err == nil {
		t.Fatal("public certificate selected a private slot")
	}
}
