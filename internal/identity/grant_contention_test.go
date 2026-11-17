package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/access"
)

// holdPolicyLock takes the named policy lock exclusively, as another writer
// would, and returns its release.
func holdPolicyLock(t *testing.T, path string) func() {
	t.Helper()
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // fixture-owned policy lock
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		})
	}
	t.Cleanup(release)
	return release
}

func TestManagedGrantWriterBoundsLockContention(t *testing.T) {
	state := t.TempDir()
	actor, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ApproveDevice(t.Context(), state, actor.ID); err != nil {
		t.Fatal(err)
	}
	current, ok := BindIdentity(state, actor.ID)
	if !ok {
		t.Fatal("approved grant missing")
	}
	release := holdPolicyLock(t, filepath.Join(state, "device-grants.lock"))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- RevokeDevice(ctx, state, actor.ID) }()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contended writer = %v, want the caller's deadline", err)
		}
	case <-time.After(access.ClassWriterBudget + 5*time.Second):
		release()
		<-finished
		t.Fatal("managed policy writer ignored its caller's deadline")
	}
	if !current() {
		t.Fatal("failed writer retired an established grant")
	}
	release()
	if err := RevokeDevice(t.Context(), state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if current() {
		t.Fatal("successful publication did not retire the original grant")
	}
}

// A policy lock that another writer holds for a second is ordinary contention
// on a loaded host; approval must wait for it rather than fail.
func TestDeviceApprovalWaitsForABrieflyHeldPolicyLock(t *testing.T) {
	locks := map[string]func(state string) string{
		"class lock": func(state string) string { return filepath.Join(state, "access", "key-classes.lock") },
		"grant lock": func(state string) string { return filepath.Join(state, "device-grants.lock") },
	}
	for name, path := range locks {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			actor, _, err := LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(state, "access"), 0o700); err != nil {
				t.Fatal(err)
			}
			release := holdPolicyLock(t, path(state))
			timer := time.AfterFunc(time.Second, release)
			defer timer.Stop()
			if err := ApproveDevice(t.Context(), state, actor.ID); err != nil {
				t.Fatalf("approval behind a briefly held %s: %v", name, err)
			}
			if _, ok := BindIdentity(state, actor.ID); !ok {
				t.Fatal("approved grant missing")
			}
		})
	}
}
