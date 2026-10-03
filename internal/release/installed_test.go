package release

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestVerifyExecutableRejectsSpecialAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(root, "large")
	file, err := os.Create(large) //nolint:gosec // sparse fixture lives in the test-owned temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maximumBinary + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link, large, root} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		done := make(chan error, 1)
		go func() { done <- VerifyExecutable(ctx, path, "digest") }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("accepted invalid executable: %s", path)
			}
		case <-ctx.Done():
			t.Fatalf("invalid executable blocked verification: %s", path)
		}
		cancel()
	}
}

func TestVerifyExecutableMatchesImageAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh")
	contents := []byte("installed mesh image")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExecutable(t.Context(), path, testDigest(contents)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExecutable(t.Context(), path, "other image"); !errors.Is(err, ErrExecutableChecksumMismatch) {
		t.Fatalf("wrong image lost integrity classification: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := VerifyExecutable(ctx, path, testDigest(contents)); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	file, err := os.Open(path) //nolint:gosec // fixture lives in the test-owned temporary directory
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // read-only
	if _, err := (executableReader{ctx: ctx, file: file}).Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("reader ignored cancellation: %v", err)
	}
}

func TestCopyExecutableMatchesImageAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh")
	contents := make([]byte, 96<<10)
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	var destination bytes.Buffer
	if err := CopyExecutable(t.Context(), path, testDigest(contents), &destination); err != nil || !bytes.Equal(contents, destination.Bytes()) {
		t.Fatalf("verified copy differs: %v", err)
	}
	if err := CopyExecutable(t.Context(), path, "other image", &destination); !errors.Is(err, ErrExecutableChecksumMismatch) {
		t.Fatalf("staged wrong image lost integrity classification: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := CopyExecutable(ctx, path, testDigest(contents), cancelImageWriter{cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy hash ignored cancellation: %v", err)
	}
}

type cancelImageWriter struct{ cancel context.CancelFunc }

func (w cancelImageWriter) Write(data []byte) (int, error) {
	w.cancel()
	return len(data), nil
}
