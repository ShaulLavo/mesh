package update

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/releasedocuments"
)

type historyFixture struct {
	host     *hostFixture
	files    map[string][]byte
	expected releasedocuments.HistoricalIdentity
}

func newHistoryFixture(t *testing.T) *historyFixture {
	t.Helper()
	f := newHostFixture(t)
	version := f.current.Version
	for number := range 9 {
		data := mustHostJSON(map[string]int{"legacy": number})
		f.files[version+"/"+discoveryDigest(data)+".json"] = data
	}
	archive, _ := fixtureDocumentBundle(t, f.files, version, f.signer)
	archive = malformedDocumentBundle(t, archive, "history-run")
	metadata := f.files[version+"/"+release.UpdateMetadataAssetName]
	manifest := f.files[version+"/"+release.ManifestAssetName]
	envelope := f.signer.envelope(t, archive, metadata, manifest)
	root := mustHostJSON(release.DiscoveryRoot{Format: 1, Epoch: 1, Head: release.AssetRef{Tag: version, Name: release.UpdateMetadataAssetName, SHA256: discoveryDigest(metadata)}})
	files := map[string][]byte{releasedocuments.ArchiveName: archive, releasedocuments.EnvelopeName: envelope, release.ManifestAssetName: manifest, release.DiscoveryRootAssetName: root}
	for key, data := range f.files {
		name, ok := strings.CutPrefix(key, version+"/")
		if ok && len(name) == 69 && strings.HasSuffix(name, ".json") {
			files[name] = data
		}
	}
	return &historyFixture{host: f, files: files, expected: releasedocuments.HistoricalIdentity{Version: version, Source: f.current.Commit, ReleaseID: 1}}
}

func (f *historyFixture) capture(t *testing.T, expected releasedocuments.HistoricalIdentity) string {
	t.Helper()
	directory := t.TempDir()
	writePhysicalSnapshot(t, directory, f.files, expected.Version, expected.Source)
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("draft.json")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	delete(value, "intake_run")
	value["id"], value["draft"], value["url"] = expected.ReleaseID, false, "https://api.github.com/repos/ShaulLavo/mesh/releases/1"
	if err := root.WriteFile("history.json", mustHostJSON(value), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove("draft.json"); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestBundleHistoryMetadataOnlyMaterialization(t *testing.T) {
	f := newHistoryFixture(t)
	capture := f.capture(t, f.expected)
	destination := filepath.Join(t.TempDir(), "logical")
	inventory, err := releasedocuments.MaterializeHistory(t.Context(), capture, f.expected, f.host.signer.trust, destination)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Binding.WriterRun != 9 || inventory.Binding.Version != f.expected.Version || inventory.Binding.Source != f.expected.Source || inventory.Binding.ReleaseID != f.expected.ReleaseID {
		t.Fatalf("history did not return authenticated binding: %+v", inventory.Binding)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	foundEnvelope := false
	for _, document := range inventory.Documents {
		data, err := root.ReadFile(document.Name)
		if err != nil || int64(len(data)) != document.Size || discoveryDigest(data) != document.SHA256 {
			t.Fatalf("logical history document %s: %v", document.Name, err)
		}
		foundEnvelope = foundEnvelope || document.Name == releasedocuments.EnvelopeName
	}
	if !foundEnvelope {
		t.Fatal("logical inventory dropped the original envelope")
	}
	if len(inventory.Documents) != 14 {
		t.Fatalf("logical closure changed: %d documents", len(inventory.Documents))
	}
}

func TestBundleHistoryRejectsExpectedIdentityMismatch(t *testing.T) {
	for _, kind := range []string{"tag", "source", "release", "missing-tag", "missing-source", "missing-release", "catalog-tag", "catalog-source", "catalog-release"} {
		t.Run(kind, func(t *testing.T) { testHistoryIdentityMismatch(t, kind) })
	}
}

func testHistoryIdentityMismatch(t *testing.T, kind string) {
	t.Helper()
	f := newHistoryFixture(t)
	expected := f.expected
	switch kind {
	case "tag", "catalog-tag":
		expected.Version = "v0.1.99"
	case "source", "catalog-source":
		expected.Source = strings.Repeat("f", 40)
	case "release", "catalog-release":
		expected.ReleaseID++
	case "missing-tag":
		expected.Version = ""
	case "missing-source":
		expected.Source = ""
	case "missing-release":
		expected.ReleaseID = 0
	}
	captured := f.expected
	if strings.HasPrefix(kind, "catalog-") {
		captured = expected
	}
	capture := f.capture(t, captured)
	assertHistoryFailure(t.Context(), t, f, capture, expected)
}

func TestBundleHistoryRequiresOriginalSubjectsAndValidCatalog(t *testing.T) {
	for _, kind := range []string{"missing-binding", "missing-run", "zero-run", "unsigned-manifest", "unsigned-metadata", "unsigned-archive", "bad-archive", "missing-manifest", "native-payload"} {
		t.Run(kind, func(t *testing.T) { testHistoryAuthenticationFailure(t, kind) })
	}
}

func testHistoryAuthenticationFailure(t *testing.T, kind string) {
	t.Helper()
	f := newHistoryFixture(t)
	archive := f.files[releasedocuments.ArchiveName]
	manifest := f.files[release.ManifestAssetName]
	metadata := f.host.files[f.expected.Version+"/"+release.UpdateMetadataAssetName]
	switch kind {
	case "missing-binding", "missing-run", "zero-run":
		archive = malformedDocumentBundle(t, archive, kind)
		f.files[releasedocuments.ArchiveName] = archive
		f.files[releasedocuments.EnvelopeName] = f.host.signer.envelope(t, archive, metadata, manifest)
	case "unsigned-manifest":
		f.files[releasedocuments.EnvelopeName] = f.host.signer.envelope(t, archive, metadata)
	case "unsigned-metadata":
		f.files[releasedocuments.EnvelopeName] = f.host.signer.envelope(t, archive, manifest)
	case "unsigned-archive":
		f.files[releasedocuments.EnvelopeName] = f.host.signer.envelope(t, metadata, manifest)
	case "bad-archive":
		f.files[releasedocuments.ArchiveName] = []byte("not gzip")
	case "missing-manifest":
		delete(f.files, release.ManifestAssetName)
	case "native-payload":
		f.files["mesh_linux_amd64.tar.gz"] = []byte("native payload")
	}
	assertHistoryFailure(t.Context(), t, f, f.capture(t, f.expected), f.expected)
}

func assertHistoryFailure(ctx context.Context, t *testing.T, f *historyFixture, capture string, expected releasedocuments.HistoricalIdentity) {
	t.Helper()
	parent := t.TempDir()
	if _, err := releasedocuments.MaterializeHistory(ctx, capture, expected, f.host.signer.trust, filepath.Join(parent, "logical")); err == nil {
		t.Fatal("invalid historical capture accepted")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed history retained owned output: %v %v", entries, err)
	}
}

func TestBundleHistoryPreservesStrictDraftRequirements(t *testing.T) {
	f := newHistoryFixture(t)
	directory := t.TempDir()
	writePhysicalSnapshot(t, directory, f.files, f.expected.Version, f.expected.Source)
	binding := releasedocuments.Binding{Version: f.expected.Version, Source: f.expected.Source, ReleaseID: 1, WriterRun: 1, Platforms: []release.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}}
	if _, err := releasedocuments.Materialize(t.Context(), directory, binding, f.host.signer.trust, filepath.Join(t.TempDir(), "draft")); err == nil || !strings.Contains(err.Error(), "required physical asset") {
		t.Fatalf("draft native requirement changed: %v", err)
	}
	binding.WriterRun = 0
	if _, err := releasedocuments.Materialize(t.Context(), directory, binding, f.host.signer.trust, filepath.Join(t.TempDir(), "zero-run")); err == nil {
		t.Fatal("draft accepted unknown writer run")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assertHistoryFailure(ctx, t, f, f.capture(t, f.expected), f.expected)
}

func mutateHistoryCatalog(data []byte, kind string) []byte {
	switch kind {
	case "history-run":
		return bytes.Replace(data, []byte(`"writerRun":1`), []byte(`"writerRun":9`), 1)
	case "zero-run":
		return bytes.Replace(data, []byte(`"writerRun":9`), []byte(`"writerRun":0`), 1)
	case "missing-run":
		return bytes.Replace(data, []byte(`"writerRun":9,`), bytes.Repeat([]byte(" "), len(`"writerRun":9,`)), 1)
	case "missing-binding":
		start := bytes.Index(data, []byte(`"binding":`))
		end := bytes.Index(data, []byte(`,"documents":`))
		if start < 0 || end < start {
			panic("fixture catalog has no binding")
		}
		return bytes.Replace(data, data[start:end+1], bytes.Repeat([]byte(" "), end+1-start), 1)
	default:
		return data
	}
}
