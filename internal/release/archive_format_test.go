package release

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManifestExactArchiveNames(t *testing.T) {
	if PublishedArchive != ArchiveGzip {
		t.Fatal("reader stage changed publication format")
	}
	for _, platform := range supportedPlatforms {
		for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
			artifact := Artifact{Platform: platform, Archive: ArchiveName(platform, format)}
			actual, err := artifact.Format()
			if err != nil || actual != format {
				t.Fatalf("%s %s: %s, %v", platform, format, actual, err)
			}
		}
	}
	for _, name := range []string{"mesh_linux_amd64.tar.xz", "mesh_linux_arm64.tar.br", "./mesh_linux_amd64.tar.br", "mesh_linux_amd64.tar.br.gz", "mesh_linux_amd64.tar.BR"} {
		artifact := Artifact{Platform: Platform{OS: "linux", Arch: "amd64"}, Archive: name}
		if _, err := artifact.Format(); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	manifest := testManifest()
	for index := range manifest.Artifacts {
		manifest.Artifacts[index].Archive = ArchiveName(manifest.Artifacts[index].Platform, ArchiveBrotli)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.Artifacts = manifest.Artifacts[:2]
	if err := manifest.Validate(); err == nil {
		t.Fatal("accepted fewer than three platforms")
	}
}

func TestCompactTargetUsesGzipBridgeWithoutRewritingHistory(t *testing.T) {
	assets, build, target := historicalBridgeFixture(t)
	for index := range target.Artifacts {
		target.Artifacts[index].Archive = ArchiveName(target.Artifacts[index].Platform, ArchiveBrotli)
	}
	data, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	assets["v0.1.159.json"] = data
	persisted := append([]byte(nil), assets["v0.1.151.json"]...)
	historical, err := decodeManifest(persisted)
	if err != nil {
		t.Fatal(err)
	}
	before := historical.Digest()
	client, _ := bridgeOrigin(t, assets, `[{"tag_name":"v0.1.159"},{"tag_name":"v0.1.151"}]`)
	hop, err := client.Bridge(t.Context(), build, target)
	if err != nil || hop.Version != "v0.1.151" || hop.Digest() != before {
		t.Fatalf("gzip bridge changed: %s %v", hop.Digest(), err)
	}
	for _, artifact := range hop.Artifacts {
		if !strings.HasSuffix(artifact.Archive, ".tar.gz") {
			t.Fatal("gzip history rewritten")
		}
	}
	recovered, err := decodeManifest(persisted)
	if err != nil || recovered.Digest() != before {
		t.Fatalf("persisted gzip digest changed: %v", err)
	}
}
