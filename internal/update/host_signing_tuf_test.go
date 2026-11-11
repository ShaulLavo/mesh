package update

import (
	"crypto"
	"testing"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	metadata "github.com/theupdateframework/go-tuf/v2/metadata"
)

func fixtureTrustProvenance(t *testing.T, fixture *signingFixture) []byte {
	t.Helper()
	signer, err := signature.LoadSigner(fixture.issuerKey, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	key, err := metadata.KeyFromPublicKey(&fixture.issuerKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	bootstrap := metadata.Root(expiry)
	for _, role := range []string{metadata.ROOT, metadata.TIMESTAMP, metadata.SNAPSHOT, metadata.TARGETS} {
		if err := bootstrap.Signed.AddKey(key, role); err != nil {
			t.Fatal(err)
		}
	}
	target, err := metadata.TargetFile().FromBytes("trusted_root.json", fixture.trust.Trusted, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	targets := metadata.Targets(expiry)
	targets.Signed.Targets["trusted_root.json"] = target
	snapshot := metadata.Snapshot(expiry)
	snapshot.Signed.Meta["targets.json"] = metadata.MetaFile(1)
	fixture.trust.BootstrapRoot = signFixtureTUF(t, bootstrap, signer)
	return mustHostJSON(map[string]any{"roots": [][]byte{}, "witness": map[string]any{"root": 1, "snapshot": signFixtureTUF(t, snapshot, signer), "targets": []map[string]any{{"name": metadata.TARGETS, "metadata": signFixtureTUF(t, targets, signer)}}}})
}

func signFixtureTUF[T metadata.Roles](t *testing.T, document *metadata.Metadata[T], signer signature.Signer) []byte {
	t.Helper()
	if _, err := document.Sign(signer); err != nil {
		t.Fatal(err)
	}
	data, err := document.ToBytes(false)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
