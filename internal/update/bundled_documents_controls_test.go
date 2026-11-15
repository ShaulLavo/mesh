package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/releasedocuments"
)

func TestBundledDocumentsRejectSignedMalformedContainers(t *testing.T) {
	f := newHostFixture(t)
	archive, _ := fixtureDocumentBundle(t, f.files, f.current.Version, f.signer)
	for _, kind := range []string{"traversal", "link", "pax", "extra", "duplicate-key", "member-hash", "gzip-trailer"} {
		t.Run(kind, func(t *testing.T) {
			bad := malformedDocumentBundle(t, archive, kind)
			err := readFixtureDocumentBundle(t, f, bad, f.signer.envelope(t, bad), f.current.Version)
			if err == nil {
				t.Fatal("signed malformed container accepted")
			}
		})
	}
}

func malformedDocumentBundle(t *testing.T, original []byte, kind string) []byte {
	t.Helper()
	if kind == "gzip-trailer" {
		return append(bytes.Clone(original), 'x')
	}
	gzipInput, err := gzip.NewReader(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gzipInput.Close() }()
	input := tar.NewReader(gzipInput)
	var encoded bytes.Buffer
	compressed := gzip.NewWriter(&encoded)
	output := tar.NewWriter(compressed)
	for {
		header, err := input.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(input)
		if err != nil {
			t.Fatal(err)
		}
		mutateBundleMember(header, &data, kind)
		if err := output.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "extra" {
		if err := output.WriteHeader(&tar.Header{Name: "extra.json", Size: 2, Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write([]byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func mutateBundleMember(header *tar.Header, data *[]byte, kind string) {
	if header.Name == "bundle.json" {
		*data = mutateHistoryCatalog(*data, kind)
		if kind == "duplicate-key" {
			*data = append([]byte(`{"schema":1,`), (*data)[1:]...)
			header.Size = int64(len(*data))
		}
		return
	}
	switch kind {
	case "traversal":
		header.Name = "../outside.json"
	case "link":
		header.Typeflag = tar.TypeSymlink
		header.Linkname = "outside"
		header.Size = 0
		*data = nil
	case "pax":
		header.Format = tar.FormatPAX
		header.PAXRecords = map[string]string{"comment": "extension"}
	case "member-hash":
		*data = bytes.Repeat([]byte("x"), len(*data))
	}
}

func readFixtureDocumentBundle(t *testing.T, f *hostFixture, archive, envelope []byte, version string) error {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, releasedocuments.ArchiveName) {
			_, _ = w.Write(archive)
			return
		}
		if strings.HasSuffix(r.URL.Path, releasedocuments.EnvelopeName) {
			_, _ = w.Write(envelope)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	store, err := releasedocuments.Open(t.Context(), release.Client{BaseURL: server.URL, HTTPClient: server.Client()}, f.signer.trust, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	_, _, err = store.Bootstrap(t.Context(), version)
	return hostError(err)
}

func TestBundledDocumentsAuthenticateBeforeDecompression(t *testing.T) {
	f := newHostFixture(t)
	archive, envelope := fixtureDocumentBundle(t, f.files, f.current.Version, f.signer)
	err := readFixtureDocumentBundle(t, f, []byte("not gzip"), envelope, f.current.Version)
	if err == nil || !strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "gzip") {
		t.Fatalf("wrong bootstrap rejection: %v", err)
	}
	err = readFixtureDocumentBundle(t, f, archive, envelope, "v0.1.999")
	if err == nil {
		t.Fatal("cross-tag bundle accepted")
	}
}

func TestBundledDocumentsCancellationCleansOwnedScratch(t *testing.T) {
	root := t.TempDir()
	store, err := releasedocuments.Open(t.Context(), release.Client{}, release.EmbeddedTrustRoot(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := store.Bootstrap(ctx, "v0.1.1"); err == nil {
		t.Fatal("canceled acquisition accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned scratch retained: %v %v", entries, err)
	}
}

func TestBundledDocumentsKeepSemanticReceiptValidation(t *testing.T) {
	f := newHostFixture(t)
	version := "v0.1.2"
	manifest := f.manifests[version]
	transition := manifest.Compatibility.Transitions[0]
	var receipt release.TransitionReceipt
	if err := json.Unmarshal(f.files[version+"/"+transition.Proof+".json"], &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.SessionsPreserved = false
	bad := mustHostJSON(receipt)
	transition.Proof = discoveryDigest(bad)
	manifest.Compatibility.Transitions[0] = transition
	f.files[version+"/"+transition.Proof+".json"] = bad
	f.files[version+"/"+release.ManifestAssetName] = mustHostJSON(manifest)
	metadata := f.metadata[version]
	metadata.Manifest = discoveryDigest(f.files[version+"/"+release.ManifestAssetName])
	f.metadata[version] = metadata
	f.signMetadata(version)
	archive, envelope := fixtureDocumentBundle(t, f.files, version, f.signer)
	if err := readFixtureDocumentBundle(t, f, archive, envelope, version); err != nil {
		t.Fatalf("semantic control did not pass authenticated container: %v", err)
	}
	if _, err := release.VerifyTransitionReceipt(bad, transition, manifest.Compatibility); err == nil {
		t.Fatal("old receipt validator accepted false preservation claim")
	}
}

func TestHostSourceOperationClosesAfterCancellation(t *testing.T) {
	f := newHostFixture(t)
	scratch := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	closed := 0
	f.updater.Source = func(ctx context.Context, current release.Build) (HostSource, release.DocumentSource, func() error, error) {
		documents, err := OpenHostDocuments(ctx, f.documents.Client, f.documents.Trust, scratch)
		if err != nil {
			return HostSource{}, release.DocumentSource{}, nil, err
		}
		source, err := documents.Current(ctx, current)
		if err != nil {
			_ = documents.Close()
			return HostSource{}, release.DocumentSource{}, nil, err
		}
		cancel()
		return source, documents.Source(), func() error { closed++; return documents.Close() }, nil
	}
	if err := f.updater.Tick(ctx); err == nil {
		t.Fatal("canceled update operation succeeded")
	}
	if closed != 1 {
		t.Fatalf("factory cleanup ran %d times", closed)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("factory backing retained after cancellation: %v %v", entries, err)
	}
}

func TestBundledDocumentsMaterializeActualPhysicalSnapshot(t *testing.T) {
	f := newHostFixture(t)
	version := f.current.Version
	for number := range 9 {
		data := mustHostJSON(map[string]int{"legacy": number})
		f.files[version+"/"+discoveryDigest(data)+".json"] = data
	}
	archive, envelope := fixtureDocumentBundle(t, f.files, version, f.signer)
	intake := t.TempDir()
	var metadata release.UpdateMetadata
	if err := json.Unmarshal(f.files[version+"/"+release.UpdateMetadataAssetName], &metadata); err != nil {
		t.Fatal(err)
	}
	root := mustHostJSON(release.DiscoveryRoot{Format: 1, Epoch: 1, Head: release.AssetRef{Tag: version, Name: release.UpdateMetadataAssetName, SHA256: discoveryDigest(f.files[version+"/"+release.UpdateMetadataAssetName])}})
	physical := map[string][]byte{releasedocuments.ArchiveName: archive, releasedocuments.EnvelopeName: envelope, release.ManifestAssetName: f.files[version+"/"+release.ManifestAssetName], release.DiscoveryRootAssetName: root, "checksums.txt": []byte("checksums"), "mesh.rb": []byte("cask")}
	for _, artifact := range f.manifests[version].Artifacts {
		physical[artifact.Archive] = f.files[version+"/"+artifact.Archive]
	}
	for key, data := range f.files {
		name, ok := strings.CutPrefix(key, version+"/")
		if ok && len(name) == 69 && strings.HasSuffix(name, ".json") {
			physical[name] = data
		}
	}
	writePhysicalSnapshot(t, intake, physical, version, f.current.Commit)
	binding := releasedocuments.Binding{Version: version, Source: f.current.Commit, ReleaseID: 1, WriterRun: 1, Platforms: []release.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}}
	destination := filepath.Join(t.TempDir(), "logical")
	inventory, err := releasedocuments.Materialize(t.Context(), intake, binding, f.signer.trust, destination)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	for _, document := range inventory.Documents {
		data, err := output.ReadFile(document.Name)
		if err != nil || discoveryDigest(data) != document.SHA256 {
			t.Fatalf("materialized member %s: %v", document.Name, err)
		}
	}
	binding.WriterRun++
	if _, err := releasedocuments.Materialize(t.Context(), intake, binding, f.signer.trust, filepath.Join(t.TempDir(), "wrong")); err == nil {
		t.Fatal("wrong writer run accepted")
	}
}

func writePhysicalSnapshot(t *testing.T, directory string, files map[string][]byte, version, source string) {
	t.Helper()
	var assets []map[string]any
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		assets = append(assets, map[string]any{"id": len(assets) + 1, "name": name, "size": len(data), "digest": "sha256:" + discoveryDigest(data), "state": "uploaded"})
	}
	value := map[string]any{"id": 1, "repository": "ShaulLavo/mesh", "intake_run": 1, "tag_name": version, "target_commitish": source, "draft": true, "assets": assets}
	if err := os.WriteFile(filepath.Join(directory, "draft.json"), mustHostJSON(value), 0600); err != nil {
		t.Fatal(err)
	}
}
