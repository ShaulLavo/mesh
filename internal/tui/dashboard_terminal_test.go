//go:build !windows

package tui

import (
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// The dashboard reads no input, so an echoing tty prints terminal query
// replies into the frame and desynchronizes the span-skipping renderer.
func TestQuietTerminalStopsEchoAndRestoresOnExit(t *testing.T) {
	controller, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = controller.Close()
		_ = tty.Close()
	})
	fd := int(tty.Fd())
	before, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		t.Fatalf("read termios: %v", err)
	}
	if before.Lflag&unix.ECHO == 0 {
		t.Fatal("fresh pty does not echo; test cannot observe the change")
	}

	restore := quietTerminal(tty)
	during, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		t.Fatalf("read quiet termios: %v", err)
	}
	if during.Lflag&(unix.ECHO|unix.ICANON) != 0 {
		t.Fatalf("dashboard terminal still echoes or buffers lines: lflag %#x", during.Lflag)
	}
	if during.Lflag&unix.ISIG == 0 {
		t.Fatal("dashboard terminal lost ISIG; ctrl+c would no longer interrupt")
	}

	restore()
	after, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		t.Fatalf("read restored termios: %v", err)
	}
	if after.Lflag != before.Lflag {
		t.Fatalf("restored lflag %#x, want %#x", after.Lflag, before.Lflag)
	}
}
