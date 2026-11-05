package sshd

import (
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type groupSignal struct {
	group  int
	signal syscall.Signal
}

// Bash exits without hanging up its jobs when a second SIGHUP arrives while it
// handles the first, so a dropped connection must signal each group once.
func TestHangupSignalsEachProcessGroupOnce(t *testing.T) {
	tests := []struct {
		name string
		args []string
		job  bool
	}{
		{name: "leader in foreground", args: []string{"sleep", "30"}},
		{name: "job in foreground", args: []string{"sh", "-c", "set -m; sleep 30; :"}, job: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertHangupSignalsOnce(t, test.args, test.job)
		})
	}
}

func assertHangupSignalsOnce(t *testing.T, args []string, job bool) {
	t.Helper()
	sent := recordGroupSignals(t)
	command, master, completed := startTerminalProcess(t, args)
	leader := command.Process.Pid
	foreground := awaitForegroundGroup(t, master, leader, job)
	hangupProcess(command.Process, master, completed)
	<-completed
	want := []groupSignal{{foreground, syscall.SIGHUP}}
	if foreground != leader {
		want = append(want, groupSignal{leader, syscall.SIGHUP})
	}
	if !slices.Equal(*sent, want) {
		t.Fatalf("hangup sent %v, want %v", *sent, want)
	}
	if _, err := master.Stat(); err != nil {
		t.Fatalf("hangup closed the caller-owned master: %v", err)
	}
}

func recordGroupSignals(t *testing.T) *[]groupSignal {
	t.Helper()
	original := signalGroup
	t.Cleanup(func() { signalGroup = original })
	var sent []groupSignal
	signalGroup = func(group int, signal syscall.Signal) {
		sent = append(sent, groupSignal{group, signal})
		original(group, signal)
	}
	return &sent
}

func startTerminalProcess(t *testing.T, args []string) (*exec.Cmd, *os.File, <-chan struct{}) {
	t.Helper()
	command := exec.Command(args[0], args[1:]...) //nolint:gosec // fixed test programs
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	master, err := pty.StartWithSize(command, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	completed := make(chan struct{})
	go func() { _ = command.Wait(); close(completed) }()
	t.Cleanup(func() { cleanupTerminalProcess(command, master, completed) })
	return command, master, completed
}

func cleanupTerminalProcess(command *exec.Cmd, master *os.File, completed <-chan struct{}) {
	if command.Process.Signal(syscall.Signal(0)) == nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Process.Kill()
	}
	_ = master.Close()
	<-completed
}

func awaitForegroundGroup(t *testing.T, master *os.File, leader int, job bool) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		foreground, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
		if err != nil {
			t.Fatalf("read foreground group: %v", err)
		}
		if (foreground != leader) == job {
			return foreground
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("terminal foreground never matched job=%v", job)
	return 0
}

func TestTerminalFixtureReapsBeforeForegroundSetup(t *testing.T) {
	var command *exec.Cmd
	// Reap the pre-fix child even when the cleanup assertion fails.
	t.Cleanup(func() {
		if command != nil && command.ProcessState == nil {
			_ = command.Wait()
		}
	})
	t.Run("setup stops before foreground lookup", func(t *testing.T) {
		command, _, _ = startTerminalProcess(t, []string{"sleep", "30"})
	})
	if command == nil {
		t.Fatal("terminal fixture did not start")
	}
	if command.ProcessState == nil {
		t.Fatal("terminal fixture cleanup returned without reaping its child")
	}
}
