package worker

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// SIGCHLD remains observable on the parent after EVFILT_PROC can no longer
// attach to an exiting child. The wakeup must still check the owned child.
func TestDarwinChildExitFromParentSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	fd, err := unix.Kqueue()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	change := unix.Kevent_t{Ident: uint64(unix.SIGCHLD), Filter: unix.EVFILT_SIGNAL, Flags: unix.EV_ADD | unix.EV_CLEAR}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// A parent signal is only a wakeup. Deliver it while the target is live,
	// then terminate the child after checking that the observer stays blocked.
	if err := unix.Kill(unix.Getpid(), unix.SIGCHLD); err != nil {
		t.Fatal(err)
	}
	observed := make(chan error, 1)
	go func() { observed <- awaitChildExit(fd, command.Process.Pid) }()
	select {
	case err := <-observed:
		t.Fatalf("parent wakeup authorized live child exit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := unix.Kill(command.Process.Pid, 0); err != nil {
		t.Fatalf("observer released child identifier: %v", err)
	}
	state, err := command.Process.Wait()
	if err != nil || state.Success() {
		t.Fatalf("sole reap: %v %v", state, err)
	}
}

func TestDarwinChildExitStateRequiresOwnedZombie(t *testing.T) {
	const pid = 123
	owned := func(state int8) *unix.KinfoProc {
		info := &unix.KinfoProc{}
		info.Proc.P_pid = pid
		info.Eproc.Ppid = int32(unix.Getpid())
		info.Proc.P_stat = state
		return info
	}
	wrongPID := owned(5)
	wrongPID.Proc.P_pid++
	wrongParent := owned(5)
	wrongParent.Eproc.Ppid++
	tests := []struct {
		name   string
		info   *unix.KinfoProc
		exited bool
		bad    bool
	}{
		{name: "missing", bad: true},
		{name: "wrong_pid", info: wrongPID, bad: true},
		{name: "wrong_parent", info: wrongParent, bad: true},
		{name: "starting", info: owned(1)},
		{name: "running", info: owned(2)},
		{name: "sleeping", info: owned(3)},
		{name: "stopped", info: owned(4)},
		{name: "owned_zombie", info: owned(5), exited: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertDarwinChildExitState(t, pid, tt.info, tt.exited, tt.bad)
		})
	}
}

func TestDarwinChildExitRejectsUnownedProcess(t *testing.T) {
	if err := observeChildExit(unix.Getpid()); err != unix.ECHILD {
		t.Fatalf("observed non-child exit: %v", err)
	}
}

func TestDarwinChildExitRejectsReapedChild(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if err := observeChildExit(command.Process.Pid); err == nil {
		t.Fatal("missing child info authorized reaping")
	}
}

func assertDarwinChildExitState(t *testing.T, pid int, info *unix.KinfoProc, wantExit, wantError bool) {
	t.Helper()
	exited, err := childExitState(pid, info)
	if exited != wantExit || (err != nil) != wantError {
		t.Fatalf("child exit state: exited=%t, err=%v", exited, err)
	}
}
