package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func sshUnixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	connections := make([]*net.UnixConn, 0, 2)
	for _, fd := range fds {
		file := os.NewFile(uintptr(fd), "ssh-test-socket")
		connection, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection.(*net.UnixConn))
	}
	t.Cleanup(func() { _ = connections[0].Close(); _ = connections[1].Close() })
	return connections[0], connections[1]
}

func TestSSHHelperDescriptorAdmissionAndCleanup(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3, 4, 32} {
		t.Run(strconv.Itoa(count), func(t *testing.T) { assertSSHDescriptorCount(t, count) })
	}
}

func assertSSHDescriptorCount(t *testing.T, count int) {
	t.Helper()
	sender, receiver := sshUnixPair(t)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()  //nolint:errcheck // fixture pipe
	defer write.Close() //nolint:errcheck // fixture pipe
	rights := make([]int, count)
	for index := range rights {
		rights[index] = int(read.Fd())
	}
	if count == 3 {
		rights[1], rights[2] = int(write.Fd()), int(write.Fd())
	}
	if _, _, err := sender.WriteMsgUnix([]byte{1}, unix.UnixRights(rights...), nil); err != nil {
		t.Fatal(err)
	}
	files, err := receiveSSHFiles(receiver)
	if count == 3 && err != nil {
		t.Fatal(err)
	}
	if count != 3 && err == nil {
		t.Fatalf("accepted %d descriptors", count)
	}
	closeSSHFiles(files)
	_ = read.Close()
	if _, err := unix.Write(int(write.Fd()), []byte{1}); !errors.Is(err, unix.EPIPE) {
		t.Fatalf("received descriptor leaked for count %d: %v", count, err)
	}
}

func TestSSHHelperRejectsFilesAndWrongPipeDirection(t *testing.T) {
	sender, receiver := sshUnixPair(t)
	file, err := os.CreateTemp(t.TempDir(), "not-a-pipe")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // fixture file
	rights := unix.UnixRights(int(file.Fd()), int(file.Fd()), int(file.Fd()))
	if _, _, err := sender.WriteMsgUnix([]byte{1}, rights, nil); err != nil {
		t.Fatal(err)
	}
	if files, err := receiveSSHFiles(receiver); err == nil {
		closeSSHFiles(files)
		t.Fatal("accepted regular files")
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()  //nolint:errcheck // fixture pipe
	defer write.Close() //nolint:errcheck // fixture pipe
	rights = unix.UnixRights(int(write.Fd()), int(read.Fd()), int(read.Fd()))
	if _, _, err := sender.WriteMsgUnix([]byte{1}, rights, nil); err != nil {
		t.Fatal(err)
	}
	if files, err := receiveSSHFiles(receiver); err == nil {
		closeSSHFiles(files)
		t.Fatal("accepted reversed pipe directions")
	}
}

func TestSSHHelperChecksPeerAndStateOwnership(t *testing.T) {
	_, receiver := sshUnixPair(t)
	if err := validateSSHPeer(receiver); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.Chmod(state, 0777); err != nil { //nolint:gosec // Verify that the local boundary rejects an unsafe directory.
		t.Fatal(err)
	}
	if lock, err := openSSHOwnerLock(state, "ssh-owner.lock"); err == nil {
		_ = lock.Close()
		t.Fatal("accepted writable-by-other-users state")
	}
	if err := os.Chmod(state, 0700); err != nil { //nolint:gosec // Restore the private fixture directory.
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(state, "elsewhere"), filepath.Join(state, "ssh-owner.lock")); err != nil {
		t.Fatal(err)
	}
	if lock, err := openSSHOwnerLock(state, "ssh-owner.lock"); err == nil {
		_ = lock.Close()
		t.Fatal("followed a lock symlink")
	}
}

func TestSSHHelperPendingHeaderLimitIncludesAcceptedSockets(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pool := &sshCommandPool{ctx: ctx, entries: make(map[sshPoolKey]*sshPoolEntry)}
	slots := make(chan struct{}, 32)
	var handlers sync.WaitGroup
	var last atomic.Int64
	t.Cleanup(func() { cancel(); handlers.Wait() })
	var pending []*net.UnixConn
	for range cap(slots) {
		sender, receiver := sshUnixPair(t)
		pending = append(pending, sender)
		admitSSHHelper(ctx, receiver, "unused", pool, slots, &handlers, &last)
	}
	sender, receiver := sshUnixPair(t)
	admitSSHHelper(ctx, receiver, "unused", pool, slots, &handlers, &last)
	_ = sender.SetReadDeadline(time.Now().Add(time.Minute))
	var result sshHelperResult
	if err := json.NewDecoder(sender).Decode(&result); err != nil || result.Code != 1 {
		t.Fatalf("pending admission: %+v, %v", result, err)
	}
	for _, conn := range pending {
		_ = conn.Close()
	}
	handlers.Wait()
	if len(slots) != 0 {
		t.Fatal("pending header slots leaked")
	}
}
