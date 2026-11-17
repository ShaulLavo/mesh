package update

import (
	"crypto/x509"
	"os"
	"testing"
	"time"

	metadata "github.com/theupdateframework/go-tuf/v2/metadata"
)

// Prepared releases are consumed by later jobs (a queued macOS runner, a
// failed-jobs rerun) for as long as their artifact is kept. On PR 479 the
// one-hour serving leaf expired while a darwin cohort was mid-chain.
func TestFixtureOriginOutlivesPreparedArtifact(t *testing.T) {
	directory := t.TempDir()
	fixture := newSigningFixture(t)
	proxy := newAdoptionNativeProxy(t, fixture, directory)
	late := time.Now().Add(preparedMaterialLifetime - time.Hour)

	// Consumers trust exactly this file through SSL_CERT_FILE.
	directoryRoot, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directoryRoot.Close() })
	roots, err := directoryRoot.ReadFile("fixture-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(roots) {
		t.Fatal("fixture-ca.pem holds no certificate")
	}
	leaf, err := x509.ParseCertificate(proxy.certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"github.com", "tuf-repo-cdn.sigstore.dev"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host, CurrentTime: late}); err != nil {
			t.Fatalf("serving chain for %s expires before its prepared artifact: %v", host, err)
		}
	}

	bootstrap, err := metadata.Root().FromBytes(fixture.trust.BootstrapRoot)
	if err != nil {
		t.Fatal(err)
	}
	timestamp, err := metadata.Timestamp().FromBytes(proxy.files["tuf/timestamp.json"])
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := metadata.Snapshot().FromBytes(proxy.files["tuf/snapshot.json"])
	if err != nil {
		t.Fatal(err)
	}
	targets, err := metadata.Targets().FromBytes(proxy.files["tuf/targets.json"])
	if err != nil {
		t.Fatal(err)
	}
	expiries := map[string]time.Time{"root": bootstrap.Signed.Expires, "timestamp": timestamp.Signed.Expires,
		"snapshot": snapshot.Signed.Expires, "targets": targets.Signed.Expires}
	for role, expires := range expiries {
		if !late.Before(expires) {
			t.Errorf("TUF %s expires at %s, before its prepared artifact", role, expires.UTC().Format(time.RFC3339))
		}
	}
}
