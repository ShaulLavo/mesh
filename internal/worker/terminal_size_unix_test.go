//go:build linux || darwin

package worker

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/xpty"
	"golang.org/x/sys/unix"
)

func TestTerminalDeviceOpenFlagsAreReadOnlyAndCannotAcquireControllingTerminal(t *testing.T) {
	if terminalDeviceOpenFlags&(unix.O_WRONLY|unix.O_RDWR) != 0 {
		t.Fatalf("terminal device open flags %#x permit writing", terminalDeviceOpenFlags)
	}
	for name, flag := range map[string]int{
		"O_NOCTTY":   unix.O_NOCTTY,
		"O_NONBLOCK": unix.O_NONBLOCK,
		"O_CLOEXEC":  unix.O_CLOEXEC,
		"O_NOFOLLOW": unix.O_NOFOLLOW,
	} {
		if terminalDeviceOpenFlags&flag == 0 {
			t.Fatalf("terminal device open flags %#x omit %s", terminalDeviceOpenFlags, name)
		}
	}
}

func TestReadSessionLeaderTerminalSizeReadsLivePTYWithoutChangingIt(t *testing.T) {
	const (
		wantCols = 93
		wantRows = 37
	)
	pty, err := xpty.NewPty(wantCols, wantRows)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := pty.Start(command); err != nil {
		_ = pty.Close()
		t.Fatal(err)
	}
	if current, ok := pty.(interface{ Slave() *os.File }); ok {
		_ = current.Slave().Close()
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = pty.Close()
	})

	deadline := time.Now().Add(time.Second)
	for {
		cols, rows, ok := ReadSessionLeaderTerminalSize(command.Process.Pid)
		if ok {
			if cols != wantCols || rows != wantRows {
				t.Fatalf("terminal size = %dx%d, want %dx%d", cols, rows, wantCols, wantRows)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("live session leader terminal size remained unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("terminal-size observation affected the session leader: %v", err)
	}
	cols, rows, err := pty.Size()
	if err != nil {
		t.Fatal(err)
	}
	if cols != wantCols || rows != wantRows {
		t.Fatalf("PTY size after observation = %dx%d, want %dx%d", cols, rows, wantCols, wantRows)
	}
}
