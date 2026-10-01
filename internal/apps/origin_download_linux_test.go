package apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openDownloads counts this process's descriptors on the app's unlinked
// download archives, so a test sees the real file rather than a cache entry.
func openDownloads(t *testing.T, f *appFixture, id string) int {
	t.Helper()
	root, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(root, "apps", id, ".download-")
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	open := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.HasPrefix(target, prefix) {
			open++
		}
	}
	return open
}

func startDownloadOf(t *testing.T, f *appFixture) Record {
	t.Helper()
	app := largeApp(t, f)
	first, err := f.origin.Handle(context.Background(), Request{Action: "download", ID: app.ID})
	if err != nil || first.Done {
		t.Fatalf("first chunk: done %v, %v", first.Done, err)
	}
	if open := openDownloads(t, f, app.ID); open != 1 {
		t.Fatalf("download holds %d archives, want 1", open)
	}
	return app
}

func TestDeleteClosesCachedDownload(t *testing.T) {
	f := newAppFixture(t)
	app := startDownloadOf(t, f)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "delete", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	if open := openDownloads(t, f, app.ID); open != 0 {
		t.Fatalf("deleted app's source stays open in %d download archives", open)
	}
}

func TestOfflineSyncExpiresIdleDownload(t *testing.T) {
	f := newAppFixture(t)
	app := startDownloadOf(t, f)
	f.now = f.now.Add(downloadIdle)
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, errors.New("edge offline") }
	_ = f.origin.Sync(context.Background())
	if open := openDownloads(t, f, app.ID); open != 0 {
		t.Fatalf("idle download stayed open through an offline sync: %d", open)
	}
}

func TestCloseReleasesCachedDownloads(t *testing.T) {
	f := newAppFixture(t)
	app := startDownloadOf(t, f)
	f.origin.Close()
	if open := openDownloads(t, f, app.ID); open != 0 {
		t.Fatalf("closed origin kept %d download archives open", open)
	}
}
