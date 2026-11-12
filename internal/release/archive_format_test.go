package release

import (
	"encoding/json"
	"testing"
)

func TestManifestExactArchiveNames(t *testing.T) {
	if PublishedArchive != ArchiveGzip {
		t.Fatal("reader stage changed publication format")
	}
	for _, platform := range supportedPlatforms {
		checkPlatformArchiveFormats(t, platform)
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

func checkPlatformArchiveFormats(t *testing.T, platform Platform) {
	t.Helper()
	for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
		artifact := Artifact{Platform: platform, Archive: ArchiveName(platform, format)}
		actual, err := artifact.Format()
		if err != nil || actual != format {
			t.Fatalf("%s %s: %s, %v", platform, format, actual, err)
		}
	}
}

func TestRouteCompactTargetKeepsGzipSourceManifest(t *testing.T) {
	graph := graphFixture(t, 2)
	source := graph.metadata[0]
	sourceRef := AssetRef{Tag: source.Version, Name: ManifestAssetName, SHA256: source.Manifest}
	persisted := string(graph.documents[sourceRef])
	target := graph.metadata[1]
	manifestRef := AssetRef{Tag: target.Version, Name: ManifestAssetName, SHA256: target.Manifest}
	manifest, err := decodeManifest(graph.documents[manifestRef])
	if err != nil {
		t.Fatal(err)
	}
	for index := range manifest.Artifacts {
		manifest.Artifacts[index].Archive = ArchiveName(manifest.Artifacts[index].Platform, ArchiveBrotli)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	target.Manifest = graph.put(target.Version, ManifestAssetName, data).SHA256
	step, err := NextHop(t.Context(), graph.builds[0], source.Position, target, RouteCursor{}, graph.source(), NewPassBudget())
	if err != nil || step.Next == nil || step.Next.Manifest.Version != target.Version {
		t.Fatalf("compact target route not verified: %+v %v", step, err)
	}
	if string(graph.documents[sourceRef]) != persisted {
		t.Fatal("planning changed the source's gzip manifest")
	}
}
