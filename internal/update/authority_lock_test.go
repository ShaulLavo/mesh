package update

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAuthorityLockConcurrentCreation(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const count = 32
	files := make(chan *os.File, count)
	results := make(chan error, count)
	start := make(chan struct{})
	var ready, workers sync.WaitGroup
	ready.Add(count)
	for range count {
		workers.Go(func() {
			ready.Done()
			<-start
			file, err := store.openAuthorityLock()
			files <- file
			results <- err
		})
	}
	ready.Wait()
	close(start)
	workers.Wait()
	close(files)
	close(results)
	for file := range files {
		if file == nil {
			continue
		}
		t.Cleanup(func() { _ = file.Close() })
	}
	for err := range results {
		if err != nil {
			t.Errorf("concurrent authority lock creation: %v", err)
		}
	}
	if t.Failed() {
		return
	}
	assertAuthorityLockExclusion(t, store)
}

func assertAuthorityLockExclusion(t *testing.T, store *Store) {
	t.Helper()
	first, err := store.openAuthorityLock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if err := unix.Flock(int(first.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	second, err := store.openAuthorityLock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	a, err := first.Stat()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Stat()
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("reopening changed lock identity: %v", err)
	}
	if err := unix.Flock(int(second.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("second authority writer acquired held lock: %v", err)
	}
}

func TestAuthorityLockRejectsUnsafeExistingFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory", "fifo", "public"} {
		t.Run(kind, func(t *testing.T) { testUnsafeAuthorityLock(t, kind) })
	}
}

func testUnsafeAuthorityLock(t *testing.T, kind string) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.directory, ".lock")
	switch kind {
	case "symlink", "hardlink":
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, nil, 0600); err != nil {
			t.Fatal(err)
		}
		link := os.Symlink
		if kind == "hardlink" {
			link = os.Link
		}
		err = link(target, path)
	case "directory":
		err = os.Mkdir(path, 0700)
	case "fifo":
		err = unix.Mkfifo(path, 0600)
	case "public":
		err = os.WriteFile(path, nil, 0600)
		if err == nil {
			err = os.Chmod(path, 0644) //nolint:gosec // the fixture must expose unsafe lock permissions to verify rejection
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.openAuthorityLock()
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("unsafe authority lock was accepted")
	}
}

func TestAuthorityLockDoesNotRecreateRemovedParent(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.directory); err != nil {
		t.Fatal(err)
	}
	file, err := store.openAuthorityLock()
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed lock directory error: %v", err)
	}
}
