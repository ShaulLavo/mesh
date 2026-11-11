package release

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	metadata "github.com/theupdateframework/go-tuf/v2/metadata"
)

func delegationFixture(t *testing.T, first string, scope string) (TrustRoot, *fixtureTransport, materialWitness) {
	t.Helper()
	trust, fixture := tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(time.Hour))
	signer, key := testTUFSigner(t)
	id, err := key.ID()
	if err != nil {
		t.Fatal(err)
	}
	top := metadata.Targets(time.Now().Add(time.Hour))
	top.Signed.Version = 2
	top.Signed.Delegations = &metadata.Delegations{Keys: map[string]*metadata.Key{id: key}, Roles: []metadata.DelegatedRole{{Name: "material", KeyIDs: []string{id}, Threshold: 1, Paths: []string{scope}}}}
	leaf := metadata.Targets(time.Now().Add(time.Hour))
	target, err := metadata.TargetFile().FromBytes(trustedMaterialTarget, trust.Trusted, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	leaf.Signed.Targets[trustedMaterialTarget] = target
	roles := []witnessedRole{{Name: metadata.TARGETS}, {Name: "material", Metadata: signedTUF(t, leaf, signer)}}
	if first != "" {
		prior := metadata.Targets(time.Now().Add(time.Hour))
		if first == "shadows" {
			prior.Signed.Targets[trustedMaterialTarget] = &metadata.TargetFiles{Length: 1, Hashes: metadata.Hashes{"sha256": make([]byte, 32)}}
		}
		top.Signed.Delegations.Roles = append([]metadata.DelegatedRole{{Name: "prior", KeyIDs: []string{id}, Threshold: 1, Paths: []string{trustedMaterialTarget}, Terminating: first == "terminates"}}, top.Signed.Delegations.Roles...)
		roles = append(roles, witnessedRole{Name: "prior", Metadata: signedTUF(t, prior, signer)})
	}
	roles[0].Metadata = signedTUF(t, top, fixture.signer)
	snapshot := metadata.Snapshot(time.Now().Add(time.Hour))
	snapshot.Signed.Version = 2
	for _, role := range roles {
		version := int64(1)
		if role.Name == metadata.TARGETS {
			version = 2
		}
		digest := sha256.Sum256(role.Metadata)
		snapshot.Signed.Meta[role.Name+".json"] = &metadata.MetaFiles{Version: version, Length: int64(len(role.Metadata)), Hashes: metadata.Hashes{"sha256": digest[:]}}
		fixture.documents[fmt.Sprintf("/%d.%s.json", version, role.Name)] = role.Metadata
	}
	snapshotBytes := signedTUF(t, snapshot, fixture.signer)
	fixture.documents["/2.snapshot.json"] = snapshotBytes
	timestamp := metadata.Timestamp(time.Now().Add(time.Hour))
	timestamp.Signed.Version = 2
	timestamp.Signed.Meta["snapshot.json"] = metadata.MetaFile(2)
	fixture.documents["/timestamp.json"] = signedTUF(t, timestamp, fixture.signer)
	return trust, fixture, materialWitness{Root: 2, Snapshot: snapshotBytes, Targets: roles}
}

func TestRefreshRetainsAndAuthenticatesActualDelegations(t *testing.T) {
	trust, fixture, _ := delegationFixture(t, "empty", trustedMaterialTarget)
	cache := t.TempDir()
	loaded := refreshFixtureTrust(t, trust, fixture, cache)
	if err := VerifyArtifactSignature(loaded, recordedSubject, recordedEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(cache, "material", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	contents, err := readTrustFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var saved cachedMaterial
	if err := json.Unmarshal(contents, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Witness.Targets) != 3 {
		t.Fatalf("discarded visited delegation metadata: %d roles", len(saved.Witness.Targets))
	}
	saved.Witness.Targets = saved.Witness.Targets[:2]
	if err := persistTrustMaterial(cache, saved.Material, saved.Witness); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTrustRoot(trust, cache); err == nil {
		t.Fatal("accepted missing earlier visited delegation")
	}
}

func TestOfflineDelegationWitnessRespectsPathAndPrecedence(t *testing.T) {
	for _, kind := range []string{"scope", "shadows", "terminates"} {
		t.Run(kind, func(t *testing.T) { assertDelegationRejected(t, kind) })
	}
}

func assertDelegationRejected(t *testing.T, kind string) {
	t.Helper()
	first, scope := kind, trustedMaterialTarget
	if kind == "scope" {
		first = ""
		scope = "different.json"
	}
	trust, fixture, witness := delegationFixture(t, first, scope)
	cache := t.TempDir()
	saveFixtureRoots(t, fixture, cache)
	if err := persistTrustMaterial(cache, trust.Trusted, witness); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTrustRoot(trust, cache); err == nil {
		t.Fatalf("accepted %s delegation witness", kind)
	}
}

func TestReusingRootKeysCannotPromoteOlderMaterial(t *testing.T) {
	trust, fixture := tufFixture(t, 2, recordedTrust().Trusted, time.Now().Add(time.Hour))
	cache := t.TempDir()
	saveFixtureRoots(t, fixture, cache)
	next, err := metadata.Root().FromBytes(fixture.documents["/2.root.json"])
	if err != nil {
		t.Fatal(err)
	}
	next.Signed.Version = 3
	next.ClearSignatures()
	if err := writeTrustFile(filepath.Join(cache, "roots", "3.root.json"), signedTUF(t, next, fixture.signer)); err != nil {
		t.Fatal(err)
	}
	witness := fixtureWitness(fixture, 1)
	witness.Root = 3
	if err := persistTrustMaterial(cache, trust.Trusted, witness); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTrustRoot(trust, cache)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.history[0].order.Root != 2 {
		t.Fatal("caller revision claim promoted old signed metadata")
	}
}
