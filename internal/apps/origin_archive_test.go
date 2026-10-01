package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func uploadDirs(t *testing.T, f *appFixture) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	return dirs
}

func TestStagingLeftByCrashDoesNotOutliveRestartOrBlockRetry(t *testing.T) {
	f := newAppFixture(t)
	upload, digest := uploadSource(t, f, sourceFixture(t))
	// A crash between unpacking and the rename leaves the extracted tree, and
	// takes its owner's lock with it.
	for _, leftover := range []string{"staging-crashed", "source-" + upload} {
		if err := os.MkdirAll(filepath.Join(f.root, "uploads", leftover), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.root, "uploads", leftover, "index.html"), []byte("original page"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	restartAppOrigin(t, f)
	if _, err := os.Stat(filepath.Join(f.root, "uploads", "staging-crashed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart kept staging a crashed commit left: %v", err)
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest}); err != nil {
		t.Fatalf("leftover staging blocked the retry: %v", err)
	}
}

func TestStartupSweepNeverFollowsSymlinkedUploads(t *testing.T) {
	f := newAppFixture(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "staging-unrelated")
	if err := os.MkdirAll(victim, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.root, "uploads")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "uploads")); err != nil {
		t.Fatal(err)
	}
	restartAppOrigin(t, f)
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Fatalf("startup sweep followed a symlinked uploads directory: %v", err)
	}
}

func TestStartupSweepSparesAnotherOriginsLiveStaging(t *testing.T) {
	f := newAppFixture(t)
	upload, digest := uploadSource(t, f, sourceFixture(t))
	entered, release := make(chan struct{}), make(chan struct{})
	f.origin.config.Exchange = func(ctx context.Context, s Signed) (Signed, error) {
		var q Request
		_ = json.Unmarshal(s.Body, &q)
		if q.Action == "allocate" {
			close(entered)
			<-release
		}
		return f.edge.Exchange(ctx, s)
	}
	created := make(chan error, 1)
	go func() {
		_, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest})
		created <- err
	}()
	<-entered
	// Another origin, with its own state, starts on the same workload root
	// while the first is between unpacking and the rename.
	if _, err := NewOrigin(context.Background(), OriginConfig{Store: newMemoryAppStore(), Key: f.otherKey, EdgeIdentity: identityFor(f.edgeKey), Exchange: f.edge.Exchange, Workers: &fakeWorkers{}, DataRoot: f.root, Now: func() time.Time { return f.now }}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-created; err != nil {
		t.Fatalf("another origin's start removed live staging: %v", err)
	}
}

func TestFailedUpdateRemovesItsStaging(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	upload, digest := uploadSource(t, f, sourceFixture(t))
	// Something already occupies the workspace, so the rename fails.
	occupied := filepath.Join(f.root, "apps", app.ID, "source-"+upload)
	if err := os.MkdirAll(filepath.Join(occupied, "stale"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "static", UploadID: upload, Digest: digest}); err == nil {
		t.Fatal("update into an occupied workspace succeeded")
	}
	if dirs := uploadDirs(t, f); len(dirs) != 0 {
		t.Fatalf("failed update left staging: %v", dirs)
	}
}

func downloadAll(t *testing.T, f *appFixture, id string) []byte {
	t.Helper()
	var archive []byte
	for {
		result, err := f.origin.Handle(context.Background(), Request{Action: "download", ID: id, Offset: int64(len(archive))})
		if err != nil {
			t.Fatal(err)
		}
		archive = append(archive, result.Data...)
		if result.Done {
			return archive
		}
	}
}

func requireSourceArchive(t *testing.T, archive []byte) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("download is not gzip: %v", err)
	}
	header, err := tar.NewReader(gz).Next()
	if err != nil || header.Name != "index.html" {
		t.Fatalf("download does not hold the source: %#v %v", header, err)
	}
}

func appDirEntries(t *testing.T, f *appFixture, id string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, "apps", id))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "source-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestDownloadArchivesDoNotFollowPlantedLinks(t *testing.T) {
	plant := map[string]func(string, string) error{"symlink": os.Symlink, "hardlink": os.Link}
	for kind, link := range plant {
		t.Run(kind, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			victim := filepath.Join(f.root, "victim")
			if err := os.WriteFile(victim, []byte("unrelated data"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"download.tar.gz", "browser-download.tar.gz"} {
				if err := link(victim, filepath.Join(f.root, "apps", app.ID, name)); err != nil {
					t.Fatal(err)
				}
			}
			requireSourceArchive(t, downloadAll(t, f, app.ID))
			response := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), true))
			if response.Code != http.StatusOK {
				t.Fatalf("browser download returned %d", response.Code)
			}
			requireSourceArchive(t, response.Body.Bytes())
			if data, err := os.ReadFile(victim); err != nil || string(data) != "unrelated data" { //nolint:gosec // Reads the test's own victim file.
				t.Fatalf("download wrote through a planted %s: %q %v", kind, data, err)
			}
		})
	}
}

func TestDownloadLeavesNoArchive(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	requireSourceArchive(t, downloadAll(t, f, app.ID))
	if response := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), true)); response.Code != http.StatusOK {
		t.Fatalf("browser download returned %d", response.Code)
	}
	if left := appDirEntries(t, f, app.ID); len(left) != 0 {
		t.Fatalf("download left archives behind: %v", left)
	}
}

// sameError fails unless every layer refused, all with the same message.
func sameError(t *testing.T, limit string, errs map[string]error) {
	t.Helper()
	for layer, err := range errs {
		if err == nil {
			t.Fatalf("%s accepted a source over the %s limit", layer, limit)
		}
	}
	var want string
	for _, err := range errs {
		if want == "" {
			want = err.Error()
		}
		if err.Error() != want {
			t.Fatalf("%s limit disagrees between layers: %v", limit, errs)
		}
	}
}

func TestArchiveSizeLimitAgreesAcrossLayers(t *testing.T) {
	f := newAppFixture(t)
	// Incompressible files under the expanded limit whose tar framing pushes
	// the compressed archive past it.
	source := t.TempDir()
	data := make([]byte, 13400)
	for i := range 5000 {
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("f%04d", i)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, packErr := Pack(context.Background(), source, io.Discard)
	begun, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin"})
	if err != nil {
		t.Fatal(err)
	}
	_, uploadErr := f.origin.Handle(context.Background(), Request{Action: "upload.chunk", UploadID: begun.UploadID, Offset: MaxArchive - 4, Data: []byte("too far")})
	oversized := filepath.Join(t.TempDir(), "oversized.tar.gz")
	if err := os.WriteFile(oversized, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversized, MaxArchive+1); err != nil {
		t.Fatal(err)
	}
	unpackErr := unpack(oversized, t.TempDir(), "")
	sameError(t, "compressed", map[string]error{"pack": packErr, "upload": uploadErr, "unpack": unpackErr})
}

func TestExpandedSizeLimitAgreesBetweenPackAndUnpack(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "big"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(source, "big"), MaxArchive+1); err != nil {
		t.Fatal(err)
	}
	_, packErr := Pack(context.Background(), source, io.Discard)
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "big", Mode: 0600, Size: MaxArchive + 1}); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "expanded.tar.gz")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	unpackErr := unpack(path, t.TempDir(), digestBytes(archive.Bytes()))
	if errors.Is(unpackErr, io.ErrUnexpectedEOF) {
		t.Fatalf("unpack read past the limit before refusing: %v", unpackErr)
	}
	sameError(t, "expanded", map[string]error{"pack": packErr, "unpack": unpackErr})
}

func TestAbandonedDownloadIsReleased(t *testing.T) {
	f := newAppFixture(t)
	source := t.TempDir()
	data := make([]byte, 2*ChunkSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "index.html"), data, 0600); err != nil {
		t.Fatal(err)
	}
	upload, digest := uploadSource(t, f, source)
	created, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.origin.Handle(context.Background(), Request{Action: "download", ID: created.App.ID})
	if err != nil || first.Done {
		t.Fatalf("first chunk: done %v, %v", first.Done, err)
	}
	f.now = f.now.Add(downloadIdle)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.origin.downloads) != 0 {
		t.Fatal("abandoned download kept its archive open")
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "download", ID: created.App.ID, Offset: int64(len(first.Data))}); err == nil || !strings.Contains(err.Error(), "start it again") {
		t.Fatalf("resumed an expired download: %v", err)
	}
}

// largeApp creates an app whose source archive spans several download chunks.
func largeApp(t *testing.T, f *appFixture) Record {
	t.Helper()
	source := t.TempDir()
	data := make([]byte, 3*ChunkSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "index.html"), data, 0600); err != nil {
		t.Fatal(err)
	}
	upload, digest := uploadSource(t, f, source)
	created, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	return *created.App
}

func TestOverlappingDownloadsBothFinish(t *testing.T) {
	f := newAppFixture(t)
	app := largeApp(t, f)
	read := func(offset int) Result {
		t.Helper()
		result, err := f.origin.Handle(context.Background(), Request{Action: "download", ID: app.ID, Offset: int64(offset)})
		if err != nil {
			t.Fatalf("chunk at %d: %v", offset, err)
		}
		return result
	}
	first, second := read(0).Data, read(0).Data
	for {
		chunk := read(len(first))
		first = append(first, chunk.Data...)
		if chunk.Done {
			break
		}
	}
	for {
		chunk := read(len(second))
		second = append(second, chunk.Data...)
		if chunk.Done {
			break
		}
	}
	if !bytes.Equal(first, second) {
		t.Fatal("overlapping downloads received different archives")
	}
	requireSourceArchive(t, second)
}

func TestDownloadStartedBeforeUpdateFinishesOnItsRevision(t *testing.T) {
	f := newAppFixture(t)
	app := largeApp(t, f)
	first, err := f.origin.Handle(context.Background(), Request{Action: "download", ID: app.ID})
	if err != nil || first.Done {
		t.Fatalf("first chunk: done %v, %v", first.Done, err)
	}
	upload, digest := uploadSource(t, f, updatedSourceArchiveFixture(t))
	if _, err := f.origin.Handle(context.Background(), Request{Action: "update", ID: app.ID, Kind: "static", UploadID: upload, Digest: digest}); err != nil {
		t.Fatal(err)
	}
	archive := first.Data
	for {
		chunk, err := f.origin.Handle(context.Background(), Request{Action: "download", ID: app.ID, Offset: int64(len(archive))})
		if err != nil {
			t.Fatalf("download begun before the update failed: %v", err)
		}
		archive = append(archive, chunk.Data...)
		if chunk.Done {
			break
		}
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(gz, 2*MaxArchive))
	}
	if err != nil {
		t.Fatalf("download begun before the update is not one intact archive: %v", err)
	}
}

func updatedSourceArchiveFixture(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "index.html"), []byte("updated page"), 0600); err != nil {
		t.Fatal(err)
	}
	return source
}
