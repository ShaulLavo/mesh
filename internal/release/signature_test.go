package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Recorded from cli/cli commit 233b470d3a189e89cf6fa69624e11c3fa9387d81,
// pkg/cmd/attestation/test/data/reusable-workflow-attestation.sigstore.json.

const recordedSubject = "49a3aa6075e0f49f82843e74b5baa614ad2a588e6675612bf108a0a008c5ac25"

func recordedTrust() TrustRoot {
	trust := EmbeddedTrustRoot()
	trust.Identity.Workflow = "https://github.com/github/artifact-attestations-workflows/.github/workflows/attest.yml@09b495c3f12c7881b3cc17209a327792065c1a1d"
	trust.Identity.Repository = "https://github.com/malancas/attest-demo"
	trust.Identity.RepositoryID = "804070735"
	trust.Identity.OwnerID = "16248153"
	return trust
}

func recordedBundle(t *testing.T) []byte {
	t.Helper()
	contents, err := os.ReadFile("testdata/github-bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func recordedEnvelope(t *testing.T) []byte {
	t.Helper()
	envelope, err := WrapBundle(recordedBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestRecordedExpiredCertificateVerifiesOffline(t *testing.T) {
	// The recorded public-good certificate expired in May 2024.
	if err := VerifyArtifactSignature(recordedTrust(), recordedSubject, recordedEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactSignature(EmbeddedTrustRoot(), recordedSubject, recordedEnvelope(t)); err == nil {
		t.Fatal("accepted another repository as Mesh")
	}
}

func TestSignaturePinsEveryIdentityField(t *testing.T) {
	cases := map[string]func(*SignerIdentity){
		"issuer":        func(i *SignerIdentity) { i.Issuer = "https://other.example" },
		"workflow":      func(i *SignerIdentity) { i.Workflow += "other" },
		"repository":    func(i *SignerIdentity) { i.Repository += "other" },
		"repository ID": func(i *SignerIdentity) { i.RepositoryID = "804070736" },
		"owner ID":      func(i *SignerIdentity) { i.OwnerID = "16248154" },
		"source ref":    func(i *SignerIdentity) { i.Ref = "refs/heads/other" },
		"runner":        func(i *SignerIdentity) { i.Runner = "self-hosted" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) { assertIdentityRejected(t, change) })
	}
}

func assertIdentityRejected(t *testing.T, change func(*SignerIdentity)) {
	t.Helper()
	trust := recordedTrust()
	change(&trust.Identity)
	if err := VerifyArtifactSignature(trust, recordedSubject, recordedEnvelope(t)); err == nil {
		t.Fatal("accepted wrong signer identity")
	}
}

func TestSignatureEnvelopeBoundsAndKinds(t *testing.T) {
	valid := recordedEnvelope(t)
	var envelope SignatureEnvelope
	if err := json.Unmarshal(valid, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Signatures = append([]Signature{{Kind: "future", Bundle: json.RawMessage(`{"future":true}`)}}, envelope.Signatures...)
	contents, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactSignature(recordedTrust(), recordedSubject, contents); err != nil {
		t.Fatal(err)
	}
	for _, contents := range [][]byte{nil, []byte(`{}`), []byte(`{"schema":1,"signatures":[{"kind":"future"}]}`), []byte(`{"schema":1,"signatures":[{"kind":"github-attestation"}]}`), bytes.Repeat([]byte(" "), MaxSignatureBytes+1)} {
		if err := VerifyArtifactSignature(recordedTrust(), recordedSubject, contents); err == nil {
			t.Fatal("accepted missing or invalid envelope")
		}
	}
	envelope.Signatures = append(envelope.Signatures, envelope.Signatures[0])
	contents, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSignature(contents); err == nil {
		t.Fatal("accepted three entries")
	}
	raw := []byte(`{"padding":"` + strings.Repeat("a", MaxBundleBytes) + `"}`)
	if _, err := WrapBundle(raw); err == nil {
		t.Fatal("wrapped oversized bundle")
	}
	contents = append([]byte(`{"schema":1,"signatures":[{"kind":"future","bundle":`), raw...)
	contents = append(contents, []byte(`}]}`)...)
	if _, err := decodeSignature(contents); err == nil {
		t.Fatal("accepted oversized unknown bundle")
	}
}

func TestWrapPreservesOriginalBundle(t *testing.T) {
	raw := recordedBundle(t)
	if !bytes.Contains(recordedEnvelope(t), raw) {
		t.Fatal("wrapper changed original evidence bytes")
	}
}

func TestSignatureRejectsAlteredSubjectAndEvidence(t *testing.T) {
	if err := VerifyArtifactSignature(recordedTrust(), strings.Repeat("a", 64), recordedEnvelope(t)); err == nil {
		t.Fatal("accepted altered subject")
	}
	for _, change := range []func(map[string]any){removeTransparencyEvidence, corruptStatementSignature, corruptLeafCertificate} {
		assertCorruptBundleRejected(t, change)
	}
}

func assertCorruptBundleRejected(t *testing.T, change func(map[string]any)) {
	t.Helper()
	var original map[string]any
	if err := json.Unmarshal(recordedBundle(t), &original); err != nil {
		t.Fatal(err)
	}
	change(original)
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := WrapBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactSignature(recordedTrust(), recordedSubject, envelope); err == nil {
		t.Fatal("accepted corrupt evidence")
	}
}

func removeTransparencyEvidence(bundle map[string]any) {
	material := bundle["verificationMaterial"].(map[string]any)
	delete(material, "tlogEntries")
	delete(material, "timestampVerificationData")
}

func corruptStatementSignature(bundle map[string]any) {
	envelope := bundle["dsseEnvelope"].(map[string]any)
	signatures := envelope["signatures"].([]any)
	signatures[0].(map[string]any)["sig"] = "AAAA"
}

func corruptLeafCertificate(bundle map[string]any) {
	material := bundle["verificationMaterial"].(map[string]any)
	if cert, ok := material["certificate"].(map[string]any); ok {
		cert["rawBytes"] = "AAAA"
		return
	}
	chain := material["x509CertificateChain"].(map[string]any)
	chain["certificates"].([]any)[0].(map[string]any)["rawBytes"] = "AAAA"
}

func TestSignedMetadataBindsManifestArchiveAndPage(t *testing.T) {
	fixture := newSigningFixture(t)
	metadata := testUpdateMetadata()
	manifest := testManifest()
	metadata.Version = manifest.Version
	metadata.Page.Tag = metadata.Version
	page := []byte(`{"priorAnchors":[]}`)
	archive := []byte("archive bytes")
	archiveHash := sha256.Sum256(archive)
	manifest.Artifacts[0].SHA256 = hex.EncodeToString(archiveHash[:])
	frozen, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestHash, pageHash := sha256.Sum256(frozen), sha256.Sum256(page)
	metadata.Manifest, metadata.Page.SHA256 = hex.EncodeToString(manifestHash[:]), hex.EncodeToString(pageHash[:])
	contents, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	envelope := fixture.envelope(t, contents, archive)
	if _, err := VerifyMetadata(fixture.trust, contents, envelope, frozen, page); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactSignature(fixture.trust, manifest.Artifacts[0].SHA256, envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMetadata(fixture.trust, contents, envelope, append(frozen, ' '), page); err == nil {
		t.Fatal("accepted altered manifest")
	}
	if _, err := VerifyMetadata(fixture.trust, contents, envelope, frozen, append(page, ' ')); err == nil {
		t.Fatal("accepted altered page")
	}
	if _, err := VerifyMetadata(fixture.trust, append(contents, ' '), envelope, frozen, page); err == nil {
		t.Fatal("accepted altered metadata")
	}
	if err := VerifySignature(fixture.trust, []byte("changed archive"), envelope); err == nil {
		t.Fatal("accepted altered archive")
	}
}

func TestSignatureRejectsAnotherCAAndLogKeys(t *testing.T) {
	fixture, other := newSigningFixture(t), newSigningFixture(t)
	subject := []byte("test artifact")
	envelope := fixture.envelope(t, subject)
	for _, field := range []string{"certificateAuthorities", "ctlogs", "tlogs"} {
		assertWrongTrustRejected(t, fixture.trust, other.trust, field, subject, envelope)
	}
}

func assertWrongTrustRejected(t *testing.T, trust, other TrustRoot, field string, subject, envelope []byte) {
	t.Helper()
	var original, wrong map[string]any
	if err := json.Unmarshal(trust.Trusted, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(other.Trusted, &wrong); err != nil {
		t.Fatal(err)
	}
	original[field] = wrong[field]
	contents, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	trust.Trusted = contents
	if err := VerifySignature(trust, subject, envelope); err == nil {
		t.Fatalf("accepted wrong %s", field)
	}
}

func TestSignatureRejectsInvalidObserverTimeAndLogProof(t *testing.T) {
	fixture := newSigningFixture(t)
	subject := []byte("timestamped artifact")
	for _, change := range []func(map[string]any){corruptObserverTime, corruptInclusionProof} {
		assertGeneratedEvidenceRejected(t, fixture, subject, change)
	}
}

func assertGeneratedEvidenceRejected(t *testing.T, fixture *signingFixture, subject []byte, change func(map[string]any)) {
	t.Helper()
	var envelope SignatureEnvelope
	if err := json.Unmarshal(fixture.envelope(t, subject), &envelope); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(envelope.Signatures[0].Bundle, &raw); err != nil {
		t.Fatal(err)
	}
	change(raw)
	contents, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := WrapBundle(contents)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(fixture.trust, subject, wrapped); err == nil {
		t.Fatal("accepted invalid observer or log proof")
	}
}

func corruptObserverTime(bundle map[string]any) {
	material := bundle["verificationMaterial"].(map[string]any)
	entry := material["tlogEntries"].([]any)[0].(map[string]any)
	entry["integratedTime"] = "1"
}

func corruptInclusionProof(bundle map[string]any) {
	material := bundle["verificationMaterial"].(map[string]any)
	entry := material["tlogEntries"].([]any)[0].(map[string]any)
	proof := entry["inclusionProof"].(map[string]any)
	proof["rootHash"] = "AAAA"
}
