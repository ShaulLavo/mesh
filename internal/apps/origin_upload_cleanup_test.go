package apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpiredUploadReclaimedWithRegistryOffline(t *testing.T) {
	t.Run("capacity", testExpiredUploadOfflineCapacity)
	t.Run("fresh upload and managed source", testExpiredUploadOfflineRetention)
}

func testExpiredUploadOfflineCapacity(t *testing.T) {
	t.Helper()
	f := newAppFixture(t)
	ids := make([]string, 32)
	for i := range ids {
		ids[i] = beginExpiryUpload(t, f)
	}
	f.now = f.now.Add(time.Hour)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin"}); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("33rd upload did not hit capacity: %v", err)
	}
	offline := errors.New("registry offline")
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, offline }
	if err := f.origin.Sync(context.Background()); !errors.Is(err, offline) {
		t.Fatalf("Sync hid the registry error: %v", err)
	}
	for _, id := range ids {
		requireExpiryUploadGone(t, f, id)
	}
	beginExpiryUpload(t, f)
}

func testExpiredUploadOfflineRetention(t *testing.T) {
	t.Helper()
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	source := filepath.Join(f.origin.state.Apps[app.ID].Root, "index.html")
	old := beginExpiryUpload(t, f)
	f.now = f.now.Add(30 * time.Minute)
	fresh := beginExpiryUpload(t, f)
	f.now = f.now.Add(30 * time.Minute)
	offline := errors.New("registry offline")
	f.origin.config.Exchange = func(context.Context, Signed) (Signed, error) { return Signed{}, offline }
	if err := f.origin.Sync(context.Background()); !errors.Is(err, offline) {
		t.Fatalf("Sync hid the registry error: %v", err)
	}
	requireExpiryUploadGone(t, f, old)
	requireExpiryUploadPresent(t, f, fresh)
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("outage cleanup removed managed source: %v", err)
	}
}

func TestExpiredUploadUnlinkFailureIsRetryable(t *testing.T) {
	f := newAppFixture(t)
	id := beginExpiryUpload(t, f)
	path := f.origin.uploadPath(id)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "obstacle"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	err := f.origin.Sync(context.Background())
	var removal *os.PathError
	if !errors.As(err, &removal) || removal.Op != "remove" || removal.Path != path {
		t.Fatalf("Sync did not report the archive removal failure: %v", err)
	}
	requireExpiryUploadPresent(t, f, id)
	if err := os.Remove(filepath.Join(path, "obstacle")); err != nil {
		t.Fatal(err)
	}
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireExpiryUploadGone(t, f, id)
	if err := f.origin.Sync(context.Background()); err != nil {
		t.Fatalf("repeated cleanup failed: %v", err)
	}
}

func beginExpiryUpload(t *testing.T, f *appFixture) string {
	t.Helper()
	result, err := f.origin.Handle(context.Background(), Request{Action: "upload.begin"})
	if err != nil {
		t.Fatal(err)
	}
	return result.UploadID
}

func requireExpiryUploadGone(t *testing.T, f *appFixture, id string) {
	t.Helper()
	if _, ok := f.origin.state.Uploads[id]; ok {
		t.Fatalf("expired upload %s still occupies an admission slot", id)
	}
	if _, err := os.Stat(f.origin.uploadPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired archive %s remains: %v", id, err)
	}
}

func requireExpiryUploadPresent(t *testing.T, f *appFixture, id string) {
	t.Helper()
	if _, ok := f.origin.state.Uploads[id]; !ok {
		t.Fatalf("upload %s lost its retry record", id)
	}
	if _, err := os.Stat(f.origin.uploadPath(id)); err != nil {
		t.Fatalf("upload %s lost its archive: %v", id, err)
	}
}
