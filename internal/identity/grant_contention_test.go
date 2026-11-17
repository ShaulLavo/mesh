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

func TestGrantPolicyRejectsSpecialFilesWithoutBlocking(t *testing.T) {
	for _, name := range []string{"authorized_keys", "device-grants.lock"} {
		for _, kind := range []string{"fifo", "symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) { checkSpecialGrantPolicyFile(t, name, kind) })
		}
	}
}

// A FIFO or symlink in place of a policy file must be refused, not opened in a
// way that blocks: with the writer budget at several seconds a blocking open
// would stall approval, so the hang cap sits above that budget.
func checkSpecialGrantPolicyFile(t *testing.T, name, kind string) {
	t.Helper()
	state := t.TempDir()
	actor, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, name)
	if kind == "fifo" {
		err = syscall.Mkfifo(path, 0o600)
	} else {
		target := filepath.Join(state, "target")
		err = os.WriteFile(target, nil, 0o600)
		if err == nil {
			err = os.Symlink(target, path)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if name == "authorized_keys" {
		if _, err := ReadAuthorizedKeys(path); err == nil {
			t.Fatal("reader accepted a special policy file")
		}
	}
	finished := make(chan error, 1)
	go func() { finished <- ApproveDevice(t.Context(), state, actor.ID) }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("writer accepted a special policy file")
		}
	case <-time.After(access.ClassWriterBudget + 10*time.Second):
		t.Fatal("writer blocked on a special policy file")
	}
}
