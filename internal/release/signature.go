package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	SignatureSchema   = 1
	GitHubAttestation = "github-attestation"
	MaxSignatureBytes = 256 << 10
	MaxBundleBytes    = 120 << 10
)

type SignatureEnvelope struct {
	Schema     int         `json:"schema"`
	Signatures []Signature `json:"signatures"`
}

type Signature struct {
	Kind   string          `json:"kind"`
	Bundle json.RawMessage `json:"bundle"`
}

// WrapBundle preserves the complete action output, including evidence unknown to this reader.
func WrapBundle(contents []byte) ([]byte, error) {
	if len(contents) > MaxBundleBytes || len(contents) == 0 || !json.Valid(contents) {
		return nil, errors.New("release: missing, oversized or invalid Sigstore bundle")
	}
	envelope := []byte(`{"schema":1,"signatures":[{"kind":"github-attestation","bundle":`)
	envelope = append(envelope, contents...)
	envelope = append(envelope, []byte(`}]}`)...)
	return envelope, nil
}

func decodeSignature(contents []byte) (SignatureEnvelope, error) {
	var envelope SignatureEnvelope
	if len(contents) == 0 || len(contents) > MaxSignatureBytes {
		return envelope, errors.New("release: missing signature or envelope exceeds 256 KiB")
	}
	if err := json.Unmarshal(contents, &envelope); err != nil {
		return envelope, fmt.Errorf("release: decode signature envelope: %w", err)
	}
	if envelope.Schema != SignatureSchema || len(envelope.Signatures) == 0 || len(envelope.Signatures) > 2 {
		return envelope, errors.New("release: unsupported signature schema or entry count")
	}
	for _, signature := range envelope.Signatures {
		if len(signature.Bundle) > MaxBundleBytes {
			return envelope, errors.New("release: signature bundle exceeds 120 KiB")
		}
	}
	return envelope, nil
}

func VerifySignature(trust TrustRoot, contents, envelope []byte) error {
	digest := sha256.Sum256(contents)
	return VerifyArtifactSignature(trust, hex.EncodeToString(digest[:]), envelope)
}

// VerifyArtifactSignature verifies the SHA-256 subject without retaining an archive in memory.
func VerifyArtifactSignature(trust TrustRoot, digest string, contents []byte) error {
	if !validDigest(digest) {
		return errors.New("release: signature subject is not a SHA-256 digest")
	}
	envelope, err := decodeSignature(contents)
	if err != nil {
		return err
	}
	material, err := trust.material()
	if err != nil {
		return err
	}
	var failures []error
	for _, signature := range envelope.Signatures {
		if signature.Kind != GitHubAttestation {
			continue
		}
		err := verifyBundle(material, trust.Identity, digest, signature.Bundle)
		if err == nil {
			return nil
		}
		failures = append(failures, err)
	}
	return fmt.Errorf("release: no valid supported signature: %w", errors.Join(append(failures, errors.New("pinned GitHub attestation required"))...))
}

func verifyBundle(material root.TrustedMaterial, identity SignerIdentity, digest string, raw []byte) error {
	var signed bundle.Bundle
	if err := json.Unmarshal(raw, &signed); err != nil {
		return fmt.Errorf("release: decode Sigstore bundle: %w", err)
	}
	if signed.GetDsseEnvelope() == nil {
		return errors.New("release: GitHub attestation must contain a signed statement")
	}
	verifier, err := verify.NewVerifier(material, verify.WithObserverTimestamps(1), verify.WithTransparencyLog(1), verify.WithSignedCertificateTimestamps(1))
	if err != nil {
		return fmt.Errorf("release: create Sigstore verifier: %w", err)
	}
	policy, err := identity.policy()
	if err != nil {
		return err
	}
	subject, _ := hex.DecodeString(digest)
	_, err = verifier.Verify(&signed, verify.NewPolicy(verify.WithArtifactDigest("sha256", subject), verify.WithCertificateIdentity(policy)))
	if err != nil {
		return fmt.Errorf("release: verify Sigstore bundle: %w", err)
	}
	return nil
}

func (identity SignerIdentity) policy() (verify.CertificateIdentity, error) {
	san, err := verify.NewSANMatcher(identity.Workflow, "")
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("release: workflow identity: %w", err)
	}
	issuer, err := verify.NewIssuerMatcher(identity.Issuer, "")
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("release: token issuer: %w", err)
	}
	policy, err := verify.NewCertificateIdentity(san, issuer, certificate.Extensions{
		BuildSignerURI:                  identity.Workflow,
		SourceRepositoryURI:             identity.Repository,
		SourceRepositoryIdentifier:      identity.RepositoryID,
		SourceRepositoryOwnerIdentifier: identity.OwnerID,
		SourceRepositoryRef:             identity.Ref,
		RunnerEnvironment:               identity.Runner,
	})
	if err != nil {
		return policy, fmt.Errorf("release: certificate policy: %w", err)
	}
	return policy, nil
}

func VerifyDigest(contents []byte, expected string) error {
	digest := sha256.Sum256(contents)
	want, err := hex.DecodeString(expected)
	if err != nil || !validDigest(expected) || !bytes.Equal(digest[:], want) {
		return errors.New("release: signed asset SHA-256 mismatch")
	}
	return nil
}

// VerifyMetadata binds the frozen manifest bytes and page bytes to signed routing metadata.
func VerifyMetadata(trust TrustRoot, contents, envelope, manifest, page []byte) (UpdateMetadata, error) {
	if err := VerifySignature(trust, contents, envelope); err != nil {
		return UpdateMetadata{}, err
	}
	metadata, err := DecodeUpdateMetadata(contents)
	if err != nil {
		return metadata, err
	}
	if metadata.Trust != trust.Version {
		return metadata, errors.New("release: unsupported metadata trust version")
	}
	if err := VerifyDigest(manifest, metadata.Manifest); err != nil {
		return metadata, err
	}
	if err := VerifyDigest(page, metadata.Page.SHA256); err != nil {
		return metadata, err
	}
	var frozen Manifest
	if err := json.Unmarshal(manifest, &frozen); err != nil {
		return metadata, fmt.Errorf("release: decode signed manifest: %w", err)
	}
	if err := frozen.Validate(); err != nil {
		return metadata, err
	}
	if frozen.Version != metadata.Version {
		return metadata, errors.New("release: signed metadata and manifest versions differ")
	}
	return metadata, nil
}
