package release

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestClientValidatesGzipEndAndTarTail(t *testing.T) {
	binary := []byte("x")
	valid := testArchive(t, binary)
	reader, err := gzip.NewReader(bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	badCRC := bytes.Clone(valid)
	badCRC[len(badCRC)-8] ^= 1
	badSize := bytes.Clone(valid)
	badSize[len(badSize)-4] ^= 1
	for _, tc := range []struct {
		name    string
		archive []byte
		invalid bool
		wantErr error
	}{
		{name: "valid", archive: valid},
		{name: "producer_padding", archive: testGzip(t, append(bytes.Clone(raw), make([]byte, 8192)...))},
		{name: "padding_at_limit", archive: testGzip(t, append(bytes.Clone(raw), make([]byte, 1<<20)...))},
		{name: "bad_crc", archive: badCRC, invalid: true, wantErr: gzip.ErrChecksum},
		{name: "bad_size", archive: badSize, invalid: true, wantErr: gzip.ErrChecksum},
		{name: "truncated_footer", archive: valid[:len(valid)-1], invalid: true, wantErr: io.ErrUnexpectedEOF},
		{name: "missing_footer", archive: valid[:len(valid)-8], invalid: true, wantErr: io.ErrUnexpectedEOF},
		{name: "decoded_junk", archive: testGzip(t, append(bytes.Clone(raw), []byte("JUNK")...)), invalid: true},
		{name: "second_member", archive: testGzip(t, append(bytes.Clone(raw[:len(raw)-1024]), raw...)), invalid: true},
		{name: "second_tar", archive: testGzip(t, append(bytes.Clone(raw), raw...)), invalid: true},
		{name: "second_gzip_tar", archive: append(bytes.Clone(valid), valid...), invalid: true},
		{name: "compressed_junk", archive: append(bytes.Clone(valid), []byte("JUNK")...), invalid: true},
		{name: "excessive_padding", archive: testGzip(t, append(bytes.Clone(raw), make([]byte, (1<<20)+1)...)), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testClientArchiveValidation(t, binary, tc.archive, tc.invalid, tc.wantErr)
		})
	}
}

func testClientArchiveValidation(t *testing.T, binary, archive []byte, invalid bool, wantErr error) {
	t.Helper()
	// Pin even malformed archive bytes in the manifest, so SHA verification cannot mask extraction failures.
	manifest := testManifestForArchive(binary, archive)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifestRequests, archiveRequests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/download/"+manifest.Version+"/"+ManifestAssetName, func(w http.ResponseWriter, _ *http.Request) {
		manifestRequests.Add(1)
		_, _ = w.Write(manifestJSON)
	})
	mux.HandleFunc("/download/"+manifest.Version+"/mesh_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		archiveRequests.Add(1)
		_, _ = w.Write(archive)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTPClient: server.Client()}
	resolved, err := client.Manifest(t.Context(), manifest.Version)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Digest() != manifest.Digest() {
		t.Fatal("resolved manifest differs from the pinned fixture")
	}
	platform := Platform{OS: "linux", Arch: "amd64"}
	cache := t.TempDir()
	root, err := cacheRoot(cache, resolved, platform)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache already exists: %v", err)
	}
	path, downloadErr := client.Download(t.Context(), resolved, platform, cache)
	if manifestRequests.Load() != 1 || archiveRequests.Load() != 1 {
		t.Fatalf("requests = manifest %d archive %d, want 1 each", manifestRequests.Load(), archiveRequests.Load())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if invalid {
		if path != "" || downloadErr == nil || len(entries) != 0 {
			t.Fatalf("invalid archive: path %q, error %v, remaining cache entries %v", path, downloadErr, entries)
		}
		if wantErr != nil && !errors.Is(downloadErr, wantErr) {
			t.Fatalf("Download() error = %v, want %v", downloadErr, wantErr)
		}
		return
	}
	if downloadErr != nil {
		t.Fatal(downloadErr)
	}
	if len(entries) != 1 || entries[0].Name() != "mesh" || path != filepath.Join(root, "mesh") {
		t.Fatalf("published path %q, cache entries %v", path, entries)
	}
	contents, err := os.ReadFile(path) //nolint:gosec // path is the test-owned cache image
	if err != nil || !bytes.Equal(contents, binary) {
		t.Fatalf("published contents %q, error %v", contents, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("published mode: %v, error %v", info, err)
	}
}

func testGzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestValidateArchiveTailHonorsExpandedLimit(t *testing.T) {
	for _, size := range []int{8, 9, 10} {
		decoded := bytes.NewReader(make([]byte, size))
		expanded := &io.LimitedReader{R: decoded, N: 9}
		err := validateArchiveTail(expanded)
		if (err == nil) != (size == 8) {
			t.Fatalf("decoded size %d with limit 8: %v", size, err)
		}
		if consumed := int64(size) - int64(decoded.Len()); consumed > 9 {
			t.Fatalf("read %d bytes beyond the expanded limit probe", consumed)
		}
	}
}
