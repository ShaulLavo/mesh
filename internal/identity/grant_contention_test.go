package identity

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestManagedGrantWriterBoundsLockContention(t *testing.T) {
	state := t.TempDir()
	actor, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	current, ok := BindIdentity(state, actor.ID)
	if !ok {
		t.Fatal("approved grant missing")
	}
	lock, err := os.OpenFile(filepath.Join(state, "device-grants.lock"), os.O_RDWR, 0o600) //nolint:gosec // fixture-owned policy lock
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- RevokeDevice(state, actor.ID) }()
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("writer bypassed exclusive policy ownership")
		}
	case <-time.After(time.Second):
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		<-finished
		t.Fatal("managed policy writer blocked beyond its contention budget")
	}
	if !current() {
		t.Fatal("failed writer retired an established grant")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := RevokeDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if current() {
		t.Fatal("successful publication did not retire the original grant")
	}
}

func TestGrantPolicyRejectsSpecialFilesWithoutBlocking(t *testing.T) {
	for _, name := range []string{"authorized_keys", "device-grants.lock"} {
		for _, kind := range []string{"fifo", "symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
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
				if err := ApproveDevice(state, actor.ID); err == nil {
					t.Fatal("writer accepted a special policy file")
				}
			})
		}
	}
}
