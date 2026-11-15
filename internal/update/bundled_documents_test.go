package update

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/releasedocuments"
)

func fixtureDocumentBundle(t *testing.T, files map[string][]byte, version string, signer *signingFixture) ([]byte, []byte) {
	t.Helper()
	directory := t.TempDir()
	var members []string
	for key, data := range files {
		name, ok := strings.CutPrefix(key, version+"/")
		if !ok || !strings.HasSuffix(name, ".json") || name == releasedocuments.EnvelopeName {
			continue
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		members = append(members, name)
	}
	var metadata release.UpdateMetadata
	if err := json.Unmarshal(files[version+"/"+release.UpdateMetadataAssetName], &metadata); err != nil {
		t.Fatal(err)
	}
	root := mustHostJSON(release.DiscoveryRoot{Format: 1, Epoch: metadata.Epoch, Head: release.AssetRef{Tag: version, Name: release.UpdateMetadataAssetName, SHA256: discoveryDigest(files[version+"/"+release.UpdateMetadataAssetName])}})
	if err := os.WriteFile(filepath.Join(directory, release.DiscoveryRootAssetName), root, 0600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(members, "\n"), release.DiscoveryRootAssetName) {
		members = append(members, release.DiscoveryRootAssetName)
	}
	var manifest release.Manifest
	if err := json.Unmarshal(files[version+"/"+release.ManifestAssetName], &manifest); err != nil {
		t.Fatal(err)
	}
	binding := releasedocuments.Binding{Version: version, Source: manifest.Commit, ReleaseID: 1, WriterRun: 1, Platforms: []release.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}}
	var archive bytes.Buffer
	if _, err := releasedocuments.Pack(t.Context(), directory, members, binding, &archive); err != nil {
		t.Fatal(err)
	}
	metadataBytes := files[version+"/"+release.UpdateMetadataAssetName]
	return archive.Bytes(), signer.envelope(t, archive.Bytes(), metadataBytes)
}

func TestBundledDocumentsSignedRoundTrip(t *testing.T) {
	f := newHostFixture(t)
	version := f.current.Version
	archive, envelope := fixtureDocumentBundle(t, f.files, version, f.signer)
	requests := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		switch r.URL.Path {
		case "/download/" + version + "/" + releasedocuments.ArchiveName:
			_, _ = w.Write(archive)
		case "/download/" + version + "/" + releasedocuments.EnvelopeName:
			_, _ = w.Write(envelope)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	scratch := t.TempDir()
	store, err := releasedocuments.Open(t.Context(), release.Client{BaseURL: server.URL, HTTPClient: server.Client()}, f.signer.trust, scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for range 3 {
		metadata, signature, err := store.Bootstrap(t.Context(), version)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(metadata, f.files[version+"/"+release.UpdateMetadataAssetName]) {
			t.Fatal("signed metadata changed")
		}
		if err := release.VerifySignature(f.signer.trust, metadata, signature); err != nil {
			t.Fatal(err)
		}
		manifest := f.files[version+"/"+release.ManifestAssetName]
		data, err := store.Read(t.Context(), release.AssetRef{Tag: version, Name: release.ManifestAssetName, SHA256: discoveryDigest(manifest)}, release.ManifestDocument)
		if err != nil || !bytes.Equal(data, manifest) {
			t.Fatalf("manifest read: %v", err)
		}
	}
	for path, count := range requests {
		if count != 1 {
			t.Fatalf("%s fetched %d times", path, count)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("physical requests: %v", requests)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned scratch retained: %v %v", entries, err)
	}
}
