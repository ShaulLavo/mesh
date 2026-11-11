package update

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/url"
	"testing"
	"time"

	ct "github.com/google/certificate-transparency-go"
	"github.com/google/certificate-transparency-go/tls"
	"github.com/google/certificate-transparency-go/x509util"
	"github.com/secure-systems-lab/go-securesystemslib/dsse"
	"github.com/shaul/mesh/internal/release"
	pbundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	pcommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	prekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"google.golang.org/protobuf/encoding/protojson"
)

type signingFixture struct {
	trust         release.TrustRoot
	issuer        *x509.Certificate
	issuerKey     *ecdsa.PrivateKey
	ctKey         *ecdsa.PrivateKey
	log           *ca.VirtualSigstore
	when          time.Time
	observerShift time.Duration
}

func newSigningFixture(t *testing.T) *signingFixture {
	t.Helper()
	issuer, issuerKey, err := ca.GenerateRootCa()
	if err != nil {
		t.Fatal(err)
	}
	ctKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	log, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	ctDER, err := x509.MarshalPKIXPublicKey(&ctKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ctID := sha256.Sum256(ctDER)
	logs := log.RekorLogs()
	for id, entry := range logs {
		entry.ID, err = hex.DecodeString(id)
		if err != nil {
			t.Fatal(err)
		}
	}
	material, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		[]root.CertificateAuthority{&root.FulcioCertificateAuthority{Root: issuer, URI: "https://fixture.invalid"}},
		map[string]*root.TransparencyLog{hex.EncodeToString(ctID[:]): {ID: ctID[:], PublicKey: &ctKey.PublicKey, HashFunc: crypto.SHA256, SignatureHashFunc: crypto.SHA256}},
		log.TimestampingAuthorities(), logs)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := json.Marshal(material)
	if err != nil {
		t.Fatal(err)
	}
	trust := release.EmbeddedTrustRoot()
	trust.Trusted = trusted
	return &signingFixture{trust: trust, issuer: issuer, issuerKey: issuerKey, ctKey: ctKey, log: log, when: time.Now().Add(-45 * time.Minute).Truncate(time.Second)}
}

func (fixture *signingFixture) envelope(t *testing.T, subjects ...[]byte) []byte {
	t.Helper()
	var entries []map[string]any
	for _, subject := range subjects {
		digest := sha256.Sum256(subject)
		entries = append(entries, map[string]any{"name": "fixture", "digest": map[string]string{"sha256": hex.EncodeToString(digest[:])}})
	}
	statement, err := json.Marshal(map[string]any{"_type": "https://in-toto.io/Statement/v1", "subject": entries, "predicateType": "https://fixture.invalid/release/v1", "predicate": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf := fixture.leaf(t, key.GetPublicKey())
	data := &sign.DSSEData{Data: statement, PayloadType: "application/vnd.in-toto+json"}
	sig, digest, err := key.SignData(t.Context(), data.PreAuthEncoding())
	if err != nil {
		t.Fatal(err)
	}
	bundle := &pbundle.Bundle{MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json"}
	data.Bundle(bundle, sig, digest, key.GetHashAlgorithm())
	envelope := &dsse.Envelope{PayloadType: data.PayloadType, Payload: base64.StdEncoding.EncodeToString(statement), Signatures: []dsse.Signature{{Sig: base64.StdEncoding.EncodeToString(sig)}}}
	observed := fixture.when.Add(fixture.observerShift)
	entry, err := fixture.log.GenerateTlogEntry(leaf, envelope, sig, observed.Unix(), true)
	if err != nil {
		t.Fatal(err)
	}
	logEntry := entry.TransparencyLogEntry()
	logID, err := fixture.log.RekorLogID()
	if err != nil {
		t.Fatal(err)
	}
	set, err := fixture.log.RekorSignPayload(tlog.RekorPayload{Body: base64.StdEncoding.EncodeToString(logEntry.CanonicalizedBody), IntegratedTime: observed.Unix(), LogIndex: 0, LogID: logID})
	if err != nil {
		t.Fatal(err)
	}
	logEntry.KindVersion = &prekor.KindVersion{Kind: "dsse", Version: "0.0.1"}
	logEntry.InclusionPromise = &prekor.InclusionPromise{SignedEntryTimestamp: set}
	bundle.VerificationMaterial = &pbundle.VerificationMaterial{Content: &pbundle.VerificationMaterial_Certificate{Certificate: &pcommon.X509Certificate{RawBytes: leaf.Raw}}, TlogEntries: []*prekor.TransparencyLogEntry{logEntry}}
	raw, err := protojson.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := release.WrapBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	return wrapped
}

func (fixture *signingFixture) leaf(t *testing.T, public crypto.PublicKey) *x509.Certificate {
	t.Helper()
	identity := fixture.trust.Identity
	san, err := url.Parse(identity.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), URIs: []*url.URL{san}, NotBefore: fixture.when.Add(-time.Minute), NotAfter: fixture.when.Add(time.Minute), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	fields := []struct {
		oid   asn1.ObjectIdentifier
		value string
	}{
		{certificate.OIDIssuerV2, identity.Issuer}, {certificate.OIDBuildSignerURI, identity.Workflow},
		{certificate.OIDRunnerEnvironment, identity.Runner}, {certificate.OIDSourceRepositoryURI, identity.Repository},
		{certificate.OIDSourceRepositoryIdentifier, identity.RepositoryID}, {certificate.OIDSourceRepositoryOwnerIdentifier, identity.OwnerID}, {certificate.OIDSourceRepositoryRef, identity.Ref},
	}
	for _, field := range fields {
		encoded, err := asn1.Marshal(field.value)
		if err != nil {
			t.Fatal(err)
		}
		template.ExtraExtensions = append(template.ExtraExtensions, pkix.Extension{Id: field.oid, Value: encoded})
	}
	precert := fixture.issue(t, template, public)
	template.ExtraExtensions = append(template.ExtraExtensions, fixture.sct(t, precert))
	return fixture.issue(t, template, public)
}

func (fixture *signingFixture) issue(t *testing.T, template *x509.Certificate, public crypto.PublicKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, fixture.issuer, public, fixture.issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func (fixture *signingFixture) sct(t *testing.T, precert *x509.Certificate) pkix.Extension {
	t.Helper()
	keyDER, err := x509.MarshalPKIXPublicKey(&fixture.ctKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyID := sha256.Sum256(keyDER)
	sct := ct.SignedCertificateTimestamp{SCTVersion: ct.V1, LogID: ct.LogID{KeyID: keyID}, Timestamp: uint64(fixture.when.UnixMilli())}
	entry := ct.LogEntry{Leaf: ct.MerkleTreeLeaf{Version: ct.V1, LeafType: ct.TimestampedEntryLeafType, TimestampedEntry: &ct.TimestampedEntry{Timestamp: sct.Timestamp, EntryType: ct.PrecertLogEntryType, PrecertEntry: &ct.PreCert{IssuerKeyHash: sha256.Sum256(fixture.issuer.RawSubjectPublicKeyInfo), TBSCertificate: precert.RawTBSCertificate}}}}
	message, err := ct.SerializeSCTSignatureInput(sct, entry)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(message)
	sig, err := fixture.ctKey.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	sct.Signature = ct.DigitallySigned{Algorithm: tls.SignatureAndHashAlgorithm{Hash: tls.SHA256, Signature: tls.ECDSA}, Signature: sig}
	list, err := x509util.MarshalSCTsIntoSCTList([]*ct.SignedCertificateTimestamp{&sct})
	if err != nil {
		t.Fatal(err)
	}
	tlsBytes, err := tls.Marshal(*list)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := asn1.Marshal(tlsBytes)
	if err != nil {
		t.Fatal(err)
	}
	return pkix.Extension{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 2}, Value: encoded}
}
