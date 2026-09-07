package release

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestManifestValidateRequiresCompleteSupportedArtifactSet(t *testing.T) {
	manifest := testManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	manifest.Artifacts = manifest.Artifacts[:2]
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "want 3") {
		t.Fatalf("incomplete Validate() error = %v", err)
	}
}

func TestManifestAllowsOnlyExactProvenTransition(t *testing.T) {
	manifest := testManifest()
	platform := Platform{OS: "linux", Arch: "amd64"}
	artifact, err := manifest.Artifact(platform)
	if err != nil {
		t.Fatal(err)
	}
	from := strings.Repeat("a", 64)
	build := Build{Version: "v0.1.38", Digest: from, Platform: platform, StateVersion: 7, WorkerProtocol: 1}
	if err := manifest.Allows(build); err == nil || !strings.Contains(err.Error(), "no tested") {
		t.Fatalf("Allows() without transition error = %v", err)
	}
	manifest.Compatibility.Transitions = []Transition{{FromDigest: from, ToDigest: artifact.BinarySHA256, Platform: platform, Proof: strings.Repeat("c", 64)}}
	manifest.Compatibility.StateReadMin = 5
	build.StateVersion = 5
	if err := manifest.Allows(build); err != nil {
		t.Fatalf("Allows() error = %v", err)
	}
	build.Modified = true
	if err := manifest.Allows(build); err != nil {
		t.Fatalf("Allows() modified exact transition error = %v", err)
	}
	build.StateVersion = 4
	if err := manifest.Allows(build); err == nil || !strings.Contains(err.Error(), "outside target read range") {
		t.Fatalf("Allows() old state error = %v", err)
	}
}

func TestManifestAllowsAlreadyInstalledArtifact(t *testing.T) {
	manifest := testManifest()
	artifact := manifest.Artifacts[0]
	build := Build{Digest: artifact.BinarySHA256, Platform: artifact.Platform, Modified: false}
	if err := manifest.Allows(build); err != nil {
		t.Fatalf("Allows() already installed error = %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{a: "v0.1.38", b: "v0.2.0", want: -1},
		{a: "v2.0.0", b: "v1.99.99", want: 1},
		{a: "v1.2.3", b: "v1.2.3", want: 0},
		{a: "v0.1.38+recovery.c2d5043", b: "v0.1.39", want: -1},
		{a: "v1.2.3+local.dirty", b: "v1.2.3", want: 0},
	}
	for _, test := range tests {
		got, err := CompareVersions(test.a, test.b)
		if err != nil || got != test.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, %v; want %d", test.a, test.b, got, err, test.want)
		}
	}
	for _, invalid := range []string{"1.2.3", "v1.2", "v1.02.3", "v1.2.3-rc1", "v1.2.3+", "v1.2.3+bad!"} {
		if _, err := CompareVersions(invalid, "v1.2.3"); err == nil {
			t.Fatalf("CompareVersions accepted %q", invalid)
		}
	}
}

func TestCurrentDigestNamesExecutingImage(t *testing.T) {
	contents, err := os.ReadFile(executingExecutablePath()) //nolint:gosec // test reads its own executable
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(contents)
	if got := Current().Digest; got != hex.EncodeToString(want[:]) {
		t.Fatalf("Current().Digest = %q, want executing image digest", got)
	}
}

func testManifest() Manifest {
	artifacts := make([]Artifact, len(supportedPlatforms))
	for index, platform := range supportedPlatforms {
		artifacts[index] = Artifact{
			Platform:     platform,
			Archive:      archiveName(platform),
			SHA256:       strings.Repeat(string(rune('1'+index)), 64),
			BinarySHA256: strings.Repeat(string(rune('4'+index)), 64),
		}
	}
	return Manifest{
		Schema:    ManifestSchema,
		Version:   "v0.2.0",
		Commit:    strings.Repeat("a", 40),
		Artifacts: artifacts,
		Compatibility: Compatibility{
			StateReadMin: 7, StateReadMax: 7, StateWrite: 7,
			WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1,
			JournalVersion: 1,
		},
	}
}
