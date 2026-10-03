package updateinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func damageMetadata(t *testing.T, path, kind string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "fifo":
		err = unix.Mkfifo(path, 0600)
	case "symlink":
		err = os.Symlink(filepath.Join(filepath.Dir(path), "missing"), path)
	case "directory":
		err = os.Mkdir(path, 0700)
	case "oversize":
		err = atomicWrite(path, []byte(strings.Repeat("x", journalLimit+1)), 0600)
	case "permissions":
		err = atomicWrite(path, []byte("{}"), 0666)
	default:
		t.Fatalf("unknown metadata damage: %s", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestHelperRecoveryMetadataDescriptorControls(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metadata")
	for _, kind := range []string{"fifo", "symlink", "directory", "oversize", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			if err := atomicWrite(path, []byte("verified"), 0600); err != nil {
				t.Fatal(err)
			}
			data, err := readMetadata(t.Context(), root, "metadata", true)
			if err != nil || string(data) != "verified" {
				t.Fatalf("known-good metadata = %q, %v", data, err)
			}
			damageMetadata(t, path, kind)
			defer func() { _ = os.Remove(path) }()
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			if _, err = readMetadata(ctx, root, "metadata", true); err == nil {
				t.Fatal("unsafe metadata accepted")
			}
			if err = ctx.Err(); err != nil {
				t.Fatalf("metadata rejection waited for the deadline: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readMetadata(ctx, root, "missing", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled metadata open = %v", err)
	}
	if err := atomicWrite(path, []byte("verified"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openMetadata(root, "metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(file) }()
	if _, err = readMetadataDescriptor(ctx.Err, file); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled descriptor read = %v", err)
	}
	if err = os.Mkdir(filepath.Join(root, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("real", filepath.Join(root, "redirect")); err != nil {
		t.Fatal(err)
	}
	if _, err = readMetadata(t.Context(), root, "redirect/metadata", true); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlinked parent metadata = %v", err)
	}
}

func TestHelperRecoveryMetadataLockedBoundaries(t *testing.T) {
	f := newRecoveryFixture(t)
	for _, boundary := range []string{"journal", "receipt", "service", "retained"} {
		t.Run(boundary, func(t *testing.T) {
			for _, kind := range []string{"fifo", "symlink", "directory", "oversize", "permissions", "cancelled"} {
				t.Run(kind, func(t *testing.T) {
					testRecoveryMetadataBoundary(t, f, boundary, kind)
				})
			}
		})
	}
	f.assertNoServiceMutation(t)
}

func testRecoveryMetadataBoundary(t *testing.T, f *recoveryFixture, boundary, kind string) {
	t.Helper()
	path := journalPath(f.engine.cfg.StateDir)
	mode := os.FileMode(0600)
	switch boundary {
	case "receipt":
		path = helperRecord(f.engine.cfg.StateDir)
	case "service":
		path, mode = f.prior.installation.ServicePath, 0644
	case "retained":
		path = filepath.Join(filepath.Dir(f.prior.installation.Executable), "installed.json")
		if err := atomicWrite(path, f.prior.receipt, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := readMetadata(t.Context(), filepath.Dir(path), filepath.Base(path), mode == 0600)
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Read()
	if err != nil {
		t.Fatal(err)
	}
	if kind != "cancelled" {
		damageMetadata(t, path, kind)
	}
	defer func() {
		_ = os.Remove(path)
		if err := atomicWrite(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	lock, err := lockInstallation(ctx, f.engine.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "cancelled" {
		cancel()
	}
	switch boundary {
	case "journal":
		err = f.engine.unchangedRecoveryJournal(ctx, status)
	case "receipt", "service":
		_, err = recoveryHelperSnapshot(ctx, f.request.Helper)
		if recoveryHelperUnchanged(ctx, f.engine.cfg.StateDir, f.prior) {
			t.Error("unchanged-state guard accepted unsafe metadata")
		}
	case "retained":
		err = retainRecoveryHelperReceipt(ctx, f.engine.cfg.StateDir, f.prior)
	}
	unlock(lock)
	if err == nil {
		t.Fatal("locked recovery boundary accepted unsafe metadata")
	}
	if kind == "cancelled" && !errors.Is(err, context.Canceled) {
		t.Fatalf("locked metadata cancellation = %v", err)
	}
	if kind != "cancelled" && ctx.Err() != nil {
		t.Fatalf("locked metadata rejection waited for the deadline: %v", ctx.Err())
	}
	// Acquiring again proves the unsafe read released the installation lock.
	lock, err = lockInstallation(t.Context(), f.engine.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	unlock(lock)
}
