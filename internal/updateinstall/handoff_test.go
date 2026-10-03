package updateinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

func TestCommittedExecutableHandoffHoldsActivationLockAndRechecksHealth(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mesh")
	contents := []byte("committed image")
	digest := sha256.Sum256(contents)
	build := release.Build{Digest: hex.EncodeToString(digest[:])}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	status := Status{Schema: 1, Phase: Committed, Settings: Settings{Executable: path}, Verified: &Health{HostID: "local", Build: build}}
	save := func() {
		t.Helper()
		data, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		if err := atomicWrite(journalPath(root), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	failedExec := errors.New("injected exec failure")
	err := WithCommittedExecutable(t.Context(), root, "local", build, func(installed string) error {
		if installed != path {
			t.Fatal("handoff changed installed path")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		lock, err := lockInstallation(ctx, root)
		if lock != nil {
			unlock(lock)
			t.Fatal("activation acquired lock during exec handoff")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("competing activation lost deadline: %v", err)
		}
		return failedExec
	})
	if !errors.Is(err, failedExec) {
		t.Fatalf("exec failure lost: %v", err)
	}
	lock, err := lockInstallation(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err = WithCommittedExecutable(ctx, root, "local", build, func(string) error { t.Fatal("exec while activation lock held"); return nil })
	if !errors.Is(err, ErrHandoffTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handoff lock deadline lost classification: %v", err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	err = WithCommittedExecutable(canceled, root, "local", build, func(string) error { t.Fatal("exec after cancellation"); return nil })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrHandoffTimeout) {
		t.Fatalf("parent cancellation marked retryable: %v", err)
	}
	unlock(lock)
	// Acquisition can succeed at the deadline; verification still needs the retry.
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()
	err = WithCommittedExecutable(expired, root, "local", build, func(string) error { t.Fatal("exec after verification deadline"); return nil })
	if !errors.Is(err, ErrHandoffTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline after acquisition marked terminal: %v", err)
	}
	err = WithCommittedExecutable(t.Context(), root, "local", build, func(string) error { return context.DeadlineExceeded })
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrHandoffTimeout) {
		t.Fatalf("exec deadline marked retryable: %v", err)
	}
	status.Phase = Validating
	save()
	if err := WithCommittedExecutable(t.Context(), root, "local", build, func(string) error { t.Fatal("handed off an uncommitted update"); return nil }); err == nil {
		t.Fatal("uncommitted handoff accepted")
	}
}
