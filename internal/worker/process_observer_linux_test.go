//go:build linux

package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/charmbracelet/x/xpty"
)

func TestFormatLinuxCommandLineIncludesArgumentsAndStaysBounded(t *testing.T) {
	raw := []byte("/usr/bin/go\x00test\x00./internal/worker\x00")
	if got := formatLinuxCommandLine(raw); got != "/usr/bin/go test ./internal/worker" {
		t.Fatalf("formatted command line = %q", got)
	}

	oversized := []byte(strings.Repeat("x", linuxProcessCommandLimit+100))
	if got := formatLinuxCommandLine(oversized); len(got) != linuxProcessCommandLimit {
		t.Fatalf("bounded command line length = %d, want %d", len(got), linuxProcessCommandLimit)
	}
}

func TestParseLinuxProcessState(t *testing.T) {
	process, ok := parseLinuxProcessState([]byte("201 (tricky ) process) S 1 123 123 0 -1\n"))
	if !ok || process != (observedProcessState{pid: 201, groupID: 123, alive: true}) {
		t.Fatalf("parsed live process = %#v, ok %v", process, ok)
	}
	process, ok = parseLinuxProcessState([]byte("202 (finished) Z 1 123 123 0 -1\n"))
	if !ok || process != (observedProcessState{pid: 202, groupID: 123, alive: false}) {
		t.Fatalf("parsed zombie process = %#v, ok %v", process, ok)
	}
	for _, malformed := range [][]byte{
		nil,
		[]byte("no process fields"),
		[]byte("0 (invalid pid) S 1 123"),
		[]byte("203 (invalid group) S 1 nope 123"),
	} {
		if process, ok := parseLinuxProcessState(malformed); ok {
			t.Fatalf("malformed process state parsed as %#v", process)
		}
	}
}

func TestParseLinuxChildProcessIDs(t *testing.T) {
	want := []int{102, 103, 104}
	if got := parseLinuxChildProcessIDs([]byte("102  103\ninvalid -1 0 104 ")); !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed child process IDs = %v, want %v", got, want)
	}
}

func TestLinuxProcessObserverFallsBackToSessionLeader(t *testing.T) {
	observation := defaultProcessObserver(-1, os.Getpid())
	if !path.IsAbs(observation.directory) {
		t.Fatalf("observed directory = %q, want absolute", observation.directory)
	}
	if observation.command == "" || len(observation.command) > linuxProcessCommandLimit {
		t.Fatalf("observed command = %q", observation.command)
	}
}

func TestLinuxProcessObserverUsesPTYForegroundProcess(t *testing.T) {
	directory := t.TempDir()
	pty, err := xpty.NewPty(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", "exec sleep 30")
	command.Dir = directory
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := pty.Start(command); err != nil {
		_ = pty.Close()
		t.Fatal(err)
	}
	if unixPTY, ok := pty.(interface{ Slave() *os.File }); ok {
		_ = unixPTY.Slave().Close()
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = pty.Close()
		_, _ = command.Process.Wait()
	})

	var observation processObservation
	waitFor(t, func() bool {
		observation = defaultProcessObserver(int(pty.Fd()), 0)
		return observation.directory != "" && strings.Contains(observation.command, "sleep 30")
	})
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	observedDirectory, err := filepath.EvalSymlinks(observation.directory)
	if err != nil {
		t.Fatal(err)
	}
	if observedDirectory != realDirectory {
		t.Fatalf("foreground directory = %q, want %q", observation.directory, directory)
	}
	if !strings.Contains(observation.command, "sleep 30") {
		t.Fatalf("foreground command = %q, want command and arguments", observation.command)
	}
}

func TestLinuxProcessObserverUsesLivePipelineMemberAfterGroupLeaderExits(t *testing.T) {
	directory := t.TempDir()
	pty, err := xpty.NewPty(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-i")
	command.Dir = directory
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := pty.Start(command); err != nil {
		_ = pty.Close()
		t.Fatal(err)
	}
	if unixPTY, ok := pty.(interface{ Slave() *os.File }); ok {
		_ = unixPTY.Slave().Close()
	}
	foregroundGroupID := 0
	t.Cleanup(func() {
		if foregroundGroupID > 0 && foregroundGroupID != command.Process.Pid {
			_ = syscall.Kill(-foregroundGroupID, syscall.SIGKILL)
		}
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = pty.Close()
		_, _ = command.Process.Wait()
	})
	if _, err := pty.Write([]byte("true | sleep 30\n")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		foregroundGroupID = foregroundProcessGroupID(int(pty.Fd()))
		if foregroundGroupID <= 0 || foregroundGroupID == command.Process.Pid {
			return false
		}
		_, err := os.Stat(fmt.Sprintf("/proc/%d", foregroundGroupID))
		return errors.Is(err, os.ErrNotExist)
	})
	observation := defaultProcessObserver(int(pty.Fd()), command.Process.Pid)
	if !strings.Contains(observation.command, "sleep 30") {
		t.Fatalf("foreground command = %q, want surviving pipeline member (group %d, shell %d)", observation.command, foregroundGroupID, command.Process.Pid)
	}
	observedDirectory, err := filepath.EvalSymlinks(observation.directory)
	if err != nil {
		t.Fatal(err)
	}
	realDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if observedDirectory != realDirectory {
		t.Fatalf("foreground directory = %q, want %q", observation.directory, directory)
	}
}
