package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreUploadPublishesOnlyCompleteVerifiedArchive(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	dest := filepath.Join(root, "target")
	writeStoreFile(t, source, []byte("new content"))
	writeStoreFile(t, dest, []byte("old content"))
	var archive bytes.Buffer
	info, err := Pack(t.Context(), source, &archive)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	t.Cleanup(func() { _ = store.Close() })
	initial := Info{Name: info.Name, Directory: info.Directory}
	storeRequest(t, store, Request{Action: "upload.begin", Path: dest, Info: initial})
	storeRequest(t, store, Request{Action: "upload.chunk", Data: archive.Bytes()})
	if got, _ := os.ReadFile(dest); string(got) != "old content" {
		t.Fatalf("destination changed before commit: %q", got)
	}
	result := storeRequest(t, store, Request{Action: "upload.commit", Info: info, Size: int64(archive.Len())})
	if !result.Done || store.active != nil {
		t.Fatalf("commit did not finish and release: %+v", result)
	}
	if got, _ := os.ReadFile(dest); string(got) != "new content" {
		t.Fatalf("destination after commit: %q", got)
	}
	assertNoTransferFiles(t, root)
}

func TestStoreInvalidUploadCleansStageAndPreservesDestination(t *testing.T) {
	for _, name := range []string{"offset", "oversize", "empty", "size", "digest", "metadata", "cancel"} {
		t.Run(name, func(t *testing.T) { testInvalidUpload(t, name) })
	}
}

func testInvalidUpload(t *testing.T, name string) {
	t.Helper()
	root := t.TempDir()
	dest := filepath.Join(root, "target")
	writeStoreFile(t, dest, []byte("original"))
	store := NewStore()
	t.Cleanup(func() { _ = store.Close() })
	info := Info{Name: "source", Digest: strings.Repeat("0", 64)}
	storeRequest(t, store, Request{Action: "upload.begin", Path: dest, Info: Info{Name: info.Name}})
	stage := store.active.file
	request, ctx := invalidUploadRequest(t, name, info)
	if _, err := store.Handle(ctx, request); err == nil {
		t.Fatal("invalid upload accepted")
	}
	if store.active != nil {
		t.Fatal("failed upload retained operation")
	}
	if _, err := stage.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("staging file was not closed: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "original" {
		t.Fatalf("failed upload changed destination: %q", got)
	}
	assertNoTransferFiles(t, root)
}

func invalidUploadRequest(t *testing.T, name string, info Info) (Request, context.Context) {
	t.Helper()
	ctx := t.Context()
	switch name {
	case "offset":
		return Request{Action: "upload.chunk", Offset: 1, Data: []byte("x")}, ctx
	case "oversize":
		return Request{Action: "upload.chunk", Data: make([]byte, ChunkSize+1)}, ctx
	case "empty":
		return Request{Action: "upload.chunk"}, ctx
	case "size":
		return Request{Action: "upload.commit", Info: info, Size: 1}, ctx
	case "metadata":
		info.Name = "changed"
		return Request{Action: "upload.commit", Info: info}, ctx
	case "cancel":
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return Request{Action: "upload.commit", Info: info}, canceled
	default:
		return Request{Action: "upload.commit", Info: info}, ctx
	}
}

func TestStoreDownloadStreamsVerifiedArchiveAndReleasesProducer(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	writeStoreFile(t, source, []byte("download payload"))
	store := NewStore()
	t.Cleanup(func() { _ = store.Close() })
	begin := storeRequest(t, store, Request{Action: "download.begin", Path: source})
	if begin.Info.Name != "source" || begin.Info.Digest != "" || begin.Info.Directory {
		t.Fatalf("begin metadata = %+v", begin)
	}
	result := storeRequest(t, store, Request{Action: "download.chunk"})
	if !result.Done || result.Size != int64(len(result.Data)) || result.Info.Digest == "" || store.active != nil {
		t.Fatalf("completed download = %+v", result)
	}
	archive, err := Temporary(filepath.Join(root, "received"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	if _, err := archive.Write(result.Data); err != nil {
		t.Fatal(err)
	}
	if err := Receive(t.Context(), archive, result.Info, filepath.Join(root, "received")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "received")); string(got) != "download payload" {
		t.Fatalf("received payload = %q", got)
	}
	assertNoTransferFiles(t, root)
}

func TestStoreDownloadCloseJoinsBlockedProducer(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	data := make([]byte, ChunkSize*3)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	writeStoreFile(t, source, data)
	store := NewStore()
	storeRequest(t, store, Request{Action: "download.begin", Path: source})
	producer := store.active.source
	first := storeRequest(t, store, Request{Action: "download.chunk"})
	if first.Done {
		t.Fatal("fixture did not block the producer beyond the first chunk")
	}
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock the producer")
	}
	select {
	case <-producer.done:
	default:
		t.Fatal("close returned before producer finished")
	}
	assertNoTransferFiles(t, root)
}

func TestStoreDownloadRequiresRecursiveAndExactOffsets(t *testing.T) {
	store := NewStore()
	t.Cleanup(func() { _ = store.Close() })
	root := t.TempDir()
	if _, err := store.Handle(t.Context(), Request{Action: "download.begin", Path: root}); err == nil {
		t.Fatal("directory downloaded without recursive")
	}
	storeRequest(t, store, Request{Action: "download.begin", Path: root, Recursive: true})
	if _, err := store.Handle(t.Context(), Request{Action: "download.chunk", Offset: 1}); err == nil || store.active != nil {
		t.Fatalf("wrong offset accepted or retained state: %v", err)
	}
}

func TestOpenSourceCancelAndSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source")
	writeStoreFile(t, path, []byte("payload"))
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSource(t.Context(), link, false); err == nil {
		t.Fatal("symbolic link accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	source, err := OpenSource(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.ReadAll(source); err == nil {
		t.Fatal("canceled source read succeeded")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
}

func storeRequest(t *testing.T, store *Store, request Request) Result {
	t.Helper()
	result, err := store.Handle(t.Context(), request)
	if err != nil {
		t.Fatalf("%s: %v", request.Action, err)
	}
	return result
}

func writeStoreFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoTransferFiles(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".mesh-copy-") {
			t.Fatalf("transfer left staged data %s", entry.Name())
		}
	}
}
