package main

import (
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestWorkerRegistryIncludesPublishedNonSeedAndNeverDropsPinnedVersions(t *testing.T) {
	directory := t.TempDir()
	first := fixtureRelease(t, directory, 179, nil)
	nonSeed := fixtureRelease(t, directory, 180, nil)
	policy := testPolicy()
	policy.WorkerHistoryFrom, policy.Workers = first.Tag, []string{first.Tag}
	workers, err := selectWorkerRegistry(policy, []publishedEvidence{first, nonSeed})
	if err != nil || len(workers) != 6 || workers[3].Version != nonSeed.Tag {
		t.Fatalf("exact registry: %+v, %v", workers, err)
	}
	if _, err := selectWorkerRegistry(policy, []publishedEvidence{nonSeed}); err == nil {
		t.Fatal("missing retained published worker was silently dropped")
	}
}

func TestRollbackWorkerCoverageUsesSourceApplicabilityAndExactDigest(t *testing.T) {
	directory := t.TempDir()
	old := fixtureRelease(t, directory, 179, nil)
	source := fixtureRelease(t, directory, 249, nil)
	future := fixtureRelease(t, directory, 255, nil)
	policy := testPolicy()
	policy.WorkerHistoryFrom, policy.Workers = old.Tag, []string{old.Tag}
	workers, err := selectWorkerRegistry(policy, []publishedEvidence{old, source, future})
	if err != nil {
		t.Fatal(err)
	}
	platform := proofPlatforms[0]
	from, _ := source.Manifest.Artifact(platform)
	prior, _ := old.Manifest.Artifact(platform)
	proof := release.RollbackProof{Workers: []string{from.BinarySHA256}}
	if err := requireWorkerCoverage(proof, source, platform, workers); err == nil {
		t.Fatal("matching protocol and current daemon digest certified an untested old worker")
	}
	proof.Workers = append(proof.Workers, prior.BinarySHA256)
	if err := requireWorkerCoverage(proof, source, platform, workers); err != nil {
		t.Fatalf("future workers incorrectly required at an older source: %v", err)
	}
}

func TestAuthenticatedWorkerRegistryCannotShrinkOrChangeProtocol(t *testing.T) {
	manifest := fixtureRelease(t, t.TempDir(), 179, nil).Manifest
	entry := workerArtifact{Version: manifest.Version, Commit: manifest.Commit, Protocol: 1, Artifact: manifest.Artifacts[0]}
	if err := compareWorkerRegistries([]workerArtifact{entry}, []workerArtifact{entry}); err != nil {
		t.Fatal(err)
	}
	if err := compareWorkerRegistries([]workerArtifact{entry}, nil); err == nil {
		t.Fatal("retained exact worker entry was removed")
	}
	changed := entry
	changed.Protocol++
	if err := compareWorkerRegistries([]workerArtifact{entry}, []workerArtifact{changed}); err == nil {
		t.Fatal("protocol substitution accepted for an existing executable")
	}
}
