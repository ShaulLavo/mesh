package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	common "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	metadata "github.com/theupdateframework/go-tuf/v2/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func fixtureWitness(fixture *fixtureTransport, version int64) materialWitness {
	return materialWitness{Root: fixture.rootVersion, Snapshot: fixture.documents[fmt.Sprintf("/%d.snapshot.json", version)], Targets: []witnessedRole{{Name: metadata.TARGETS, Metadata: fixture.documents[fmt.Sprintf("/%d.targets.json", version)]}}}
}

func saveFixtureRoots(t *testing.T, fixture *fixtureTransport, cache string) {
	t.Helper()
	for version := int64(2); version <= fixture.rootVersion; version++ {
		name := fmt.Sprintf("%d.root.json", version)
		if err := writeTrustFile(filepath.Join(cache, "roots", name), fixture.documents["/"+name]); err != nil {
			t.Fatal(err)
		}
	}
}

func saveFixtureMaterial(t *testing.T, fixture *fixtureTransport, cache string, contents []byte, version int64) {
	t.Helper()
	fixtureTargetsRevision(t, fixture, fixture.signer, contents, time.Now().Add(time.Hour), version)
	saveFixtureRoots(t, fixture, cache)
	if err := persistTrustMaterial(cache, contents, fixtureWitness(fixture, version)); err != nil {
		t.Fatal(err)
	}
}

func TestCacheRejectsSelfHashedForeignMaterialWithoutWitness(t *testing.T) {
	foreign := newSigningFixture(t).trust.Trusted
	cache := t.TempDir()
	digest := sha256.Sum256(foreign)
	path := filepath.Join(cache, "material", hex.EncodeToString(digest[:])+".json")
	if err := writeTrustFile(path, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustRoot(cache); err == nil {
		t.Fatal("accepted self-hashed raw foreign material")
	}
	if err := persistTrustMaterial(cache, foreign, materialWitness{}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustRoot(cache); err == nil {
		t.Fatal("accepted well-formed foreign material with no witness")
	}
}

func TestCacheRejectsSignedWitnessForAnotherDigestOrLength(t *testing.T) {
	for _, change := range []string{"digest", "length"} {
		t.Run(change, func(t *testing.T) { assertWitnessMismatch(t, change) })
	}
}

func assertWitnessMismatch(t *testing.T, change string) {
	t.Helper()
	trust, fixture := tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(time.Hour))
	contents := trust.Trusted
	if change == "digest" {
		contents = bytes.Replace(contents, []byte("sigstore.dev"), []byte("sigstorx.dev"), 1)
	}
	if change == "length" {
		contents = append(contents, ' ')
	}
	cache := t.TempDir()
	saveFixtureRoots(t, fixture, cache)
	if err := persistTrustMaterial(cache, contents, fixtureWitness(fixture, 1)); err != nil {
		t.Fatal(err)
	}
	_, err := loadTrustRoot(trust, cache)
	if err == nil || !strings.Contains(err.Error(), "target mismatch") {
		t.Fatalf("accepted mismatched authenticated target: %v", err)
	}
}

func TestAuthenticatedArchivedMaterialLoadsWithoutCurrentFreshness(t *testing.T) {
	trust, fixture := tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(time.Hour))
	fixtureTargetsRevision(t, fixture, fixture.signer, trust.Trusted, time.Now().Add(-time.Hour), 1)
	cache := t.TempDir()
	saveFixtureRoots(t, fixture, cache)
	if err := persistTrustMaterial(cache, trust.Trusted, fixtureWitness(fixture, 1)); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactSignature(loaded, recordedSubject, recordedEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	_, err = refreshTrust(context.Background(), trust, t.TempDir(), TrustRefreshBudget{Documents: 8, Duration: time.Second}, &http.Client{Transport: fixture})
	if err == nil {
		t.Fatal("online refresh treated expired metadata as current")
	}
}

func TestCacheRejectsForeignSavedRootChain(t *testing.T) {
	trust, fixture := tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(time.Hour))
	cache := t.TempDir()
	saveFixtureMaterial(t, fixture, cache, trust.Trusted, 1)
	foreign, _ := fixtureRoot(t, 2, nil, time.Now().Add(time.Hour))
	if err := writeTrustFile(filepath.Join(cache, "roots", "2.root.json"), foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTrustRoot(trust, cache); err == nil {
		t.Fatal("accepted material through an unbound root chain")
	}
}

func TestFreshRefreshIgnoresCallerSelectedSigstoreClientCache(t *testing.T) {
	trust, fixture := tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(time.Hour))
	_, foreign := tufFixture(t, 2, newSigningFixture(t).trust.Trusted, time.Now().Add(time.Hour))
	cache := t.TempDir()
	clientCache := filepath.Join(cache, "tuf", tuf.URLToPath(tuf.DefaultMirror))
	for _, name := range []string{"root", "timestamp", "snapshot", "targets"} {
		contents := foreign.documents["/1."+name+".json"]
		if name == "root" {
			contents = foreign.documents["/2.root.json"]
		}
		if name == "timestamp" {
			contents = foreign.documents["/timestamp.json"]
		}
		if err := writeTrustFile(filepath.Join(clientCache, name+".json"), contents); err != nil {
			t.Fatal(err)
		}
	}
	loaded := refreshFixtureTrust(t, trust, fixture, cache)
	if err := VerifyArtifactSignature(loaded, recordedSubject, recordedEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(fixture.requests, " "), "/timestamp.json /1.snapshot.json /1.targets.json") {
		t.Fatalf("did not perform fresh authenticated metadata work: %v", fixture.requests)
	}
}

func materialWithEnd(t *testing.T, contents []byte, kind string, end time.Time) []byte {
	t.Helper()
	parsed, err := root.NewTrustedRootProtobuf(contents)
	if err != nil {
		t.Fatal(err)
	}
	period := &common.TimeRange{Start: timestamppb.New(time.Unix(0, 0)), End: timestamppb.New(end)}
	switch kind {
	case "ca":
		parsed.CertificateAuthorities[0].ValidFor = period
	case "tsa":
		parsed.TimestampAuthorities[0].ValidFor = period
	case "ct":
		parsed.Ctlogs[0].PublicKey.ValidFor = period
	case "rekor":
		parsed.Tlogs[0].PublicKey.ValidFor = period
	default:
		t.Fatal("unknown authority kind")
	}
	modified, err := protojson.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func materialHashOrder(t *testing.T, older, newer []byte, olderSortsLast bool) []byte {
	t.Helper()
	want := sha256.Sum256(newer)
	for attempt := 0; attempt < 1000; attempt++ {
		digest := sha256.Sum256(older)
		if (hex.EncodeToString(digest[:]) > hex.EncodeToString(want[:])) == olderSortsLast {
			return older
		}
		older = append(older, ' ')
	}
	t.Fatal("could not select fixture filename order")
	return nil
}

func TestNewestAuthenticatedValidityWinsRegardlessOfFilenames(t *testing.T) {
	for _, kind := range []string{"ca", "ct", "rekor"} {
		t.Run(kind, func(t *testing.T) { assertNewestValidity(t, kind, false); assertNewestValidity(t, kind, true) })
	}
}

func assertNewestValidity(t *testing.T, kind string, oldSortsLast bool) {
	t.Helper()
	signing := newSigningFixture(t)
	subject := []byte("archived authorization")
	before := signing.envelope(t, subject)
	boundary := signing.when.Add(5 * time.Minute)
	signing.when = signing.when.Add(10 * time.Minute)
	after := signing.envelope(t, subject)
	newer := materialWithEnd(t, signing.trust.Trusted, kind, boundary)
	older := materialHashOrder(t, append([]byte(nil), signing.trust.Trusted...), newer, oldSortsLast)
	trust, fixture := tufFixture(t, 2, older, time.Now().Add(time.Hour))
	trust.Trusted = signing.trust.Trusted
	trust.Identity = signing.trust.Identity
	cache := t.TempDir()
	saveFixtureMaterial(t, fixture, cache, newer, 2)
	saveFixtureMaterial(t, fixture, cache, older, 1)
	loaded, err := loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(loaded, subject, before); err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(loaded, subject, after); err == nil {
		t.Fatalf("restored older open-ended %s authorization", kind)
	}
	extended := materialWithEnd(t, signing.trust.Trusted, kind, boundary.Add(10*time.Minute))
	saveFixtureMaterial(t, fixture, cache, extended, 3)
	loaded, err = loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(loaded, subject, after); err != nil {
		t.Fatalf("rejected newer legitimate validity extension: %v", err)
	}
}

func TestOlderCachedMaterialCannotUndoEmbeddedRestrictions(t *testing.T) {
	signing := newSigningFixture(t)
	trust, fixture := tufFixture(t, 2, signing.trust.Trusted, time.Now().Add(time.Hour))
	trust.Identity = signing.trust.Identity
	subject := []byte("embedded floor")
	before := signing.envelope(t, subject)
	end := signing.when.Add(5 * time.Minute)
	signing.when = signing.when.Add(10 * time.Minute)
	after := signing.envelope(t, subject)
	trust.Trusted = materialWithEnd(t, signing.trust.Trusted, "ca", end)
	fixtureTargetsRevision(t, fixture, fixture.signer, trust.Trusted, time.Now().Add(time.Hour), 2)
	provenance := embeddedProvenance{Roots: [][]byte{fixture.documents["/2.root.json"]}, Witness: fixtureWitness(fixture, 2)}
	var err error
	trust.provenance, err = json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	saveFixtureMaterial(t, fixture, cache, signing.trust.Trusted, 1)
	loaded, err := loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(loaded, subject, before); err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(loaded, subject, after); err == nil {
		t.Fatal("older cached target undid embedded CA restriction")
	}
}

func TestAuthenticatedKeyRotationRetainsDistinctHistoricalKeys(t *testing.T) {
	old, current := newSigningFixture(t), newSigningFixture(t)
	trust, fixture := tufFixture(t, 2, old.trust.Trusted, time.Now().Add(time.Hour))
	trust.Trusted = current.trust.Trusted
	trust.Identity = current.trust.Identity
	cache := t.TempDir()
	saveFixtureMaterial(t, fixture, cache, current.trust.Trusted, 2)
	saveFixtureMaterial(t, fixture, cache, old.trust.Trusted, 1)
	loaded, err := loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	subject := []byte("key rotation")
	if err := VerifySignature(loaded, subject, old.envelope(t, subject)); err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(loaded, subject, current.envelope(t, subject)); err != nil {
		t.Fatal(err)
	}
}

func TestNewestAuthenticatedTSADefinitionCanCloseAndExtend(t *testing.T) {
	signing := newSigningFixture(t)
	trust, fixture := tufFixture(t, 2, signing.trust.Trusted, time.Now().Add(time.Hour))
	trust.Trusted = signing.trust.Trusted
	cache := t.TempDir()
	signature := []byte("timestamped signature")
	response, err := signing.log.TimestampResponse(signature)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := trust.material()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseline.TimestampingAuthorities()[0].Verify(response, signature); err != nil {
		t.Fatal(err)
	}
	closed := materialWithEnd(t, trust.Trusted, "tsa", time.Now().Add(-10*time.Minute))
	saveFixtureMaterial(t, fixture, cache, trust.Trusted, 1)
	saveFixtureMaterial(t, fixture, cache, closed, 2)
	loaded, err := loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	material, err := loaded.material()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := material.TimestampingAuthorities()[0].Verify(response, signature); err == nil {
		t.Fatal("restored older TSA validity")
	}
	extended := materialWithEnd(t, trust.Trusted, "tsa", time.Now().Add(time.Hour))
	saveFixtureMaterial(t, fixture, cache, extended, 3)
	loaded, err = loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	material, err = loaded.material()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := material.TimestampingAuthorities()[0].Verify(response, signature); err != nil {
		t.Fatalf("rejected newer TSA extension: %v", err)
	}
}

func TestUnauthenticatedWitnessRevisionCannotInflateOrdering(t *testing.T) {
	trust, fixture := tufFixture(t, 3, recordedTrust().Trusted, time.Now().Add(time.Hour))
	// Root 3 cannot authenticate root 2's snapshot/targets, even if the saved witness claims 3.
	_, earlier := tufFixture(t, 2, trust.Trusted, time.Now().Add(time.Hour))
	cache := t.TempDir()
	saveFixtureRoots(t, fixture, cache)
	witness := fixtureWitness(earlier, 1)
	witness.Root = 3
	if err := persistTrustMaterial(cache, trust.Trusted, witness); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTrustRoot(trust, cache); err == nil {
		t.Fatal("accepted witness through unrelated authorizing keys")
	}
}

func TestCacheMaterialFilenameIsStillBound(t *testing.T) {
	trust, fixture := tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(time.Hour))
	cache := t.TempDir()
	saveFixtureMaterial(t, fixture, cache, trust.Trusted, 1)
	paths, err := filepath.Glob(filepath.Join(cache, "material", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(paths[0], filepath.Join(cache, "material", strings.Repeat("a", 64)+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTrustRoot(trust, cache); err == nil {
		t.Fatal("accepted renamed material contents")
	}
}
