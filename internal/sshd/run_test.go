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
			sent := recordGroupSignals(t)
			command, master := startTerminalProcess(t, test.args)
			leader := command.Process.Pid
			foreground := awaitForegroundGroup(t, master, leader, test.job)
			completed := make(chan struct{})
			go func() { _ = command.Wait(); close(completed) }()
			hangupProcess(command.Process, master, completed)
			<-completed
			want := []groupSignal{{foreground, syscall.SIGHUP}}
			if foreground != leader {
				want = append(want, groupSignal{leader, syscall.SIGHUP})
			}
			if !slices.Equal(*sent, want) {
				t.Fatalf("hangup sent %v, want %v", *sent, want)
			}
		})
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

func startTerminalProcess(t *testing.T, args []string) (*exec.Cmd, *os.File) {
	t.Helper()
	command := exec.Command(args[0], args[1:]...) //nolint:gosec // fixed test programs
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	master, err := pty.StartWithSize(command, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = master.Close()
	})
	return command, master
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
