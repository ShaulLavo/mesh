package sshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/creack/pty"
	"github.com/shaul/mesh/internal/terminal"
	"golang.org/x/sys/unix"
)

func TestDirectTerminalControlsPreservePollability(t *testing.T) {
	command := exec.Command("sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	master, err := startProcessTerminal(command, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan struct{})
	go func() { _ = command.Wait(); close(completed) }()
	t.Cleanup(func() { cleanupTerminalProcess(command, master, completed) })
	if got := foregroundGroup(command.Process.Pid, master); got != command.Process.Pid {
		t.Fatalf("foreground group = %d, want %d", got, command.Process.Pid)
	}
	state := &windowState{}
	size := terminal.Size{Cols: 101, Rows: 37}
	state.latest.Store(&size)
	resizeProcess(master, state)
	raw, err := master.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var actual *unix.Winsize
	var controlErr error
	err = raw.Control(func(fd uintptr) {
		flags, controlErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if controlErr == nil {
			actual, controlErr = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
		}
	})
	if err != nil || controlErr != nil {
		t.Fatalf("terminal controls: %v, %v", err, controlErr)
	}
	if flags&unix.O_NONBLOCK == 0 || actual.Col != 101 || actual.Row != 37 {
		t.Fatalf("terminal controls changed pollability or lost resize: flags=%x, size=%+v", flags, actual)
	}
}

func TestDirectDisconnectWithRetainedSlave(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprintf("leader_exits_before_disconnect=%t", before), func(t *testing.T) {
			assertRetainedSlaveDisconnect(t, before)
		})
	}
}

func TestDirectDisconnectReleasesBlockedRelays(t *testing.T) {
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprintf("pty=%t", pty), func(t *testing.T) {
			assertBlockedDirectRelays(t, pty)
		})
	}
}

func assertBlockedDirectRelays(t *testing.T, pty bool) {
	t.Helper()
	server, config := sessionTestServer(t, func(context.Context, Session) (int, error) { return 0, nil })
	handler := server.Handler
	finished := make(chan struct{})
	server.Handler = func(session charmssh.Session) { defer close(finished); handler(session) }
	client := testsession.New(t, server, config)
	input := keepInputOpen(t, client)
	output, err := client.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if pty {
		if err := client.RequestPty("xterm", 24, 80, nil); err != nil {
			t.Fatal(err)
		}
	}
	request, err := json.Marshal(RunRequest{Args: []string{"sh", "-c", "while :; do printf '%4096s' x; done"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start("run " + string(request)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(output, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		_, _ = io.CopyN(input, zeroReader{}, 8<<20)
	}()
	awaitDirectBackpressure(t)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	waitForSessionEnd(t, finished)
	waitForSessionEnd(t, inputDone)
}

func awaitDirectBackpressure(t *testing.T) {
	t.Helper()
	stack := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		goroutines := strings.Split(string(stack[:runtime.Stack(stack, true)]), "\n\n")
		input := slices.ContainsFunc(goroutines, func(trace string) bool {
			return strings.Contains(trace, "sshd.relayProcessInput") && strings.Contains(trace, "internal/poll.(*FD).Write")
		})
		output := slices.ContainsFunc(goroutines, func(trace string) bool {
			server := strings.Contains(trace, "sshd.runProcess.func") || strings.Contains(trace, "os/exec.(*Cmd).writerDescriptor.func")
			return server && strings.Contains(trace, "golang.org/x/crypto/ssh.(*window).reserve")
		})
		if input && output {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("direct SSH fixture did not block both input and output relays")
}

type zeroReader struct{}

func (zeroReader) Read(data []byte) (int, error) {
	clear(data)
	return len(data), nil
}

func assertRetainedSlaveDisconnect(t *testing.T, before bool) {
	t.Helper()
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	server, config := sessionTestServer(t, func(context.Context, Session) (int, error) { return 0, nil })
	handler := server.Handler
	finished := make(chan struct{})
	server.Handler = func(session charmssh.Session) { defer close(finished); handler(session) }
	client := testsession.New(t, server, config)
	// Teardown owns the descendant even if the SSH handler never returns.
	t.Cleanup(func() { killRetainedSlave(t, root) })
	keepInputOpen(t, client)
	if err := client.RequestPty("xterm", 24, 80, nil); err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(RunRequest{Args: []string{os.Args[0], "-test.run=^TestRetainedSlaveHelper$", "--", directory, "leader"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start("run " + string(request)); err != nil {
		t.Fatal(err)
	}
	awaitRetainedMarker(t, root, "holder")
	if err := client.WindowChange(37, 101); err != nil {
		t.Fatal(err)
	}
	if err := client.Signal("USR1"); err != nil {
		t.Fatal(err)
	}
	if before {
		if err := root.WriteFile("exit", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		awaitRetainedMarker(t, root, "exiting")
		leader, err := strconv.Atoi(string(awaitRetainedMarker(t, root, "leader")))
		if err != nil {
			t.Fatal(err)
		}
		awaitRetainedExit(t, leader)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("direct SSH handler retained its PTY relay after disconnect")
	}
}

func awaitRetainedMarker(t *testing.T, root *os.Root, name string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := root.ReadFile(name)
		if err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("retained-slave fixture did not write %s", name)
	return nil
}

func killRetainedSlave(t *testing.T, root *os.Root) {
	t.Helper()
	data, err := root.ReadFile("holder")
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Error(err)
		return
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Error(err)
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Error(err)
		return
	}
	defer process.Release() //nolint:errcheck // fixture owns only this process handle
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
	}
	awaitRetainedExit(t, pid)
	t.Logf("owned retained-slave descendant %d removed", pid)
}

func awaitRetainedExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("retained-slave fixture process %d was not reaped", pid)
}

func TestRetainedSlaveHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	directory, role := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGUSR1)
	if role == "holder" {
		holdRetainedSlave(root)
		return
	}
	if err := root.WriteFile("leader", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRetainedSlaveHelper$", "--", directory, "holder") //nolint:gosec // test helper in the current executable
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	awaitRetainedMarker(t, root, "holder")
	for {
		if _, err := root.Stat("exit"); err == nil {
			_ = root.WriteFile("exiting", []byte("7"), 0o600)
			os.Exit(7)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func holdRetainedSlave(root *os.Root) {
	signal.Ignore(syscall.SIGHUP)
	if err := root.WriteFile("holder", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	for {
		if _, err := root.Stat("holder"); errors.Is(err, os.ErrNotExist) {
			os.Exit(0)
		}
		time.Sleep(time.Second)
	}
}

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
