package update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func fixtureDocumentBundle(t *testing.T, files map[string][]byte, version string, signer *signingFixture) ([]byte, []byte) {
	t.Helper()
	archive, envelope, err := tryFixtureDocumentBundle(t.Context(), t, files, version, signer)
	if err != nil {
		t.Fatal(err)
	}
	return archive, envelope
}

func tryFixtureDocumentBundle(ctx context.Context, t *testing.T, files map[string][]byte, version string, signer *signingFixture) ([]byte, []byte, error) {
	t.Helper()
	directory := t.TempDir()
	rootDirectory, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, hostError(err)
	}
	defer func() { _ = rootDirectory.Close() }()
	var members []string
	for key, data := range files {
		name, ok := strings.CutPrefix(key, version+"/")
		if !ok || !strings.HasSuffix(name, ".json") || name == releasedocuments.EnvelopeName {
			continue
		}
		if err := rootDirectory.WriteFile(name, data, 0600); err != nil {
			return nil, nil, hostError(err)
		}
		members = append(members, name)
	}
	var metadata release.UpdateMetadata
	if err := json.Unmarshal(files[version+"/"+release.UpdateMetadataAssetName], &metadata); err != nil {
		return nil, nil, hostError(err)
	}
	root := mustHostJSON(release.DiscoveryRoot{Format: 1, Epoch: metadata.Epoch, Head: release.AssetRef{Tag: version, Name: release.UpdateMetadataAssetName, SHA256: discoveryDigest(files[version+"/"+release.UpdateMetadataAssetName])}})
	if original, exists := files[version+"/"+release.DiscoveryRootAssetName]; exists {
		root = original
	}
	if err := os.WriteFile(filepath.Join(directory, release.DiscoveryRootAssetName), root, 0600); err != nil {
		return nil, nil, hostError(err)
	}
	if !strings.Contains(strings.Join(members, "\n"), release.DiscoveryRootAssetName) {
		members = append(members, release.DiscoveryRootAssetName)
	}
	var manifest release.Manifest
	if err := json.Unmarshal(files[version+"/"+release.ManifestAssetName], &manifest); err != nil {
		return nil, nil, hostError(err)
	}
	binding := releasedocuments.Binding{Version: version, Source: manifest.Commit, ReleaseID: 1, WriterRun: 1, Platforms: []release.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}}
	var archive bytes.Buffer
	if _, err := releasedocuments.Pack(ctx, directory, members, binding, &archive); err != nil {
		return nil, nil, hostError(err)
	}
	var original release.SignatureEnvelope
	if raw := files[version+"/"+releasedocuments.EnvelopeName]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &original); err != nil {
			return nil, nil, hostError(err)
		}
	}
	if original.Schema == 0 {
		original.Schema = 1
	}
	var container release.SignatureEnvelope
	if err := json.Unmarshal(signer.envelopeContext(ctx, t, archive.Bytes()), &container); err != nil {
		return nil, nil, hostError(err)
	}
	original.Signatures = append(original.Signatures, container.Signatures...)
	return archive.Bytes(), mustHostJSON(original), nil
}

func serveFixtureBundle(ctx context.Context, t *testing.T, w http.ResponseWriter, key string, files map[string][]byte, signer *signingFixture) bool {
	version, name, ok := strings.Cut(key, "/")
	if !ok || name != releasedocuments.ArchiveName && name != releasedocuments.EnvelopeName {
		return false
	}
	if _, exists := files[version+"/"+release.UpdateMetadataAssetName]; !exists {
		http.NotFound(w, nil)
		return true
	}
	archive, envelope, err := tryFixtureDocumentBundle(ctx, t, files, version, signer)
	if err != nil {
		http.Error(w, "invalid fixture publication", http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if name == releasedocuments.ArchiveName {
		_, _ = io.Copy(w, bytes.NewReader(archive))
		return true
	}
	_, _ = io.Copy(w, bytes.NewReader(envelope))
	return true
}

func TestBundledDocumentsSignedRoundTrip(t *testing.T) {
	for _, count := range []int{1, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) { testBundledDocumentsRoundTrip(t, count) })
	}
}

func testBundledDocumentsRoundTrip(t *testing.T, count int) {
	f := newHostFixture(t)
	version := f.current.Version
	for number := range count {
		data := mustHostJSON(map[string]int{"logicalDocument": number})
		f.files[version+"/"+discoveryDigest(data)+".worker-group.json"] = data
	}
	archive, envelope := fixtureDocumentBundle(t, f.files, version, f.signer)
	requests := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		switch r.URL.Path {
		case "/download/" + version + "/" + releasedocuments.ArchiveName:
			_, _ = io.Copy(w, bytes.NewReader(archive))
		case "/download/" + version + "/" + releasedocuments.EnvelopeName:
			_, _ = io.Copy(w, bytes.NewReader(envelope))
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
