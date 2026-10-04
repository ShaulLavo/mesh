package machinename

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestReviewerPrivateCacheDirectory(t *testing.T) {
	for _, mode := range []string{"world-writable", "derived-symlink"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, cacheDirectory)
			reviewerUnsafeCacheDirectory(t, root, dir, mode)
			claim := cacheFixtureClaim(t, "destination", 1)
			if _, err := RememberClaim(t.Context(), root, claim.ID, claim); err == nil {
				t.Fatal("accepted an unsafe derived cache directory and published a claim")
			}
		})
	}
}

func TestReviewerCacheReadRefusesDerivedDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 1)
	if _, err := RememberClaim(t.Context(), root, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "moved-cache")
	cache := filepath.Join(root, cacheDirectory)
	if err := os.Rename(cache, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cache); err != nil {
		t.Fatal(err)
	}
	if _, err := CachedClaim(root, claim.ID); err == nil {
		t.Fatal("read a cache claim through an escaping derived-directory symlink")
	}
}

func TestReviewerProjectionSuffixCollision(t *testing.T) {
	a, b := make([]byte, 32), make([]byte, 32)
	a[0], b[0] = 1, 2
	claims := []Claim{
		{ID: base64.RawURLEncoding.EncodeToString(a), MachineName: "shared", Revision: 1},
		{ID: base64.RawURLEncoding.EncodeToString(b), MachineName: "shared", Revision: 1},
	}
	for _, claim := range claims {
		if err := ValidateClaim(claim.ID, claim); err != nil {
			t.Fatal(err)
		}
	}
	rows := Project(claims)
	if !reflect.DeepEqual(rows, Project([]Claim{claims[1], claims[0]})) {
		t.Fatal("order-dependent projection")
	}
	if !rows[0].Conflict || !rows[1].Conflict || !rows[0].Priority || rows[1].Priority {
		t.Fatal("incorrect conflict priority")
	}
	if rows[0].Suffix == rows[1].Suffix {
		t.Fatal("two distinct valid stable identities have the same duplicate-name suffix")
	}
}

func TestReviewerCacheSpecialFilesAndBoundedLock(t *testing.T) {
	for _, kind := range []string{"record-fifo", "lock-fifo", "lock-timeout"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			claim := cacheFixtureClaim(t, "destination", 1)
			if _, err := RememberClaim(t.Context(), root, claim.ID, claim); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(root, cacheDirectory, claim.ID+".json")
			lock := filepath.Join(root, cacheDirectory, claim.ID+".lock")
			reviewerUnsafeCacheFile(t, record, lock, kind)
			started := time.Now()
			claim.Revision++
			_, err := RememberClaim(context.Background(), root, claim.ID, claim)
			if err == nil {
				t.Fatal("unsafe file or contended writer accepted")
			}
			if kind == "lock-timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lock error lacks deadline: %v", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("cache operation exceeded its bounded lock admission")
			}
		})
	}
}

func reviewerUnsafeCacheDirectory(t *testing.T, root, dir, mode string) {
	t.Helper()
	if mode == "world-writable" {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		//nolint:gosec // negative fixture must exercise rejection of world-writable cache directories
		if err := os.Chmod(dir, 0777); err != nil {
			t.Fatal(err)
		}
		return
	}
	destination := filepath.Join(root, "other")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(destination, dir); err != nil {
		t.Fatal(err)
	}
}

func reviewerUnsafeCacheFile(t *testing.T, record, lock, kind string) {
	t.Helper()
	if kind == "lock-timeout" {
		file, err := os.OpenFile(lock, os.O_RDWR, 0) //nolint:gosec // private fixture lock descriptor
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := record
	if kind == "lock-fifo" {
		path = lock
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClaimCacheRefusesContainedRecordAndLockSymlinks(t *testing.T) {
	for _, extension := range []string{".json", ".lock"} {
		t.Run(extension, func(t *testing.T) {
			directory := t.TempDir()
			claim := cacheFixtureClaim(t, "destination", 1)
			if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(filepath.Join(directory, cacheDirectory))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close() //nolint:errcheck // fixture directory descriptor cleanup
			filename := claim.ID + extension
			if err := root.Rename(filename, "contained-original"+extension); err != nil {
				t.Fatal(err)
			}
			if err := root.Symlink("contained-original"+extension, filename); err != nil {
				t.Fatal(err)
			}
			claim.Revision++
			if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err == nil {
				t.Fatal("accepted a contained final-file symlink")
			}
			if extension == ".json" {
				if _, err := CachedClaim(directory, claim.ID); err == nil {
					t.Fatal("reader accepted a contained record symlink")
				}
			}
		})
	}
}

func TestClaimCacheConcurrentInitialLockCreationSharesInode(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, cacheDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Join(directory, cacheDirectory))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck // fixture directory descriptor cleanup
	start := make(chan struct{})
	results := make(chan *os.File, 64)
	for range cap(results) {
		go func() {
			<-start
			file, err := openPrivateCacheFile(root, "destination.lock", os.O_RDWR, true)
			if err != nil {
				t.Error(err)
			}
			results <- file
		}()
	}
	close(start)
	var first os.FileInfo
	for range cap(results) {
		file := <-results
		if file == nil {
			continue
		}
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = info
		}
		if !os.SameFile(first, info) {
			t.Error("concurrent lock creation opened different lock inodes")
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
