//go:build !windows

package tui

import (
	"os"

	"golang.org/x/sys/unix"
)

// quietTerminal stops the tty echoing input while the dashboard draws. The
// dashboard reads no keys, so the line discipline would otherwise print
// terminal query replies and stray keystrokes into the frame, and the
// span-skipping renderer would keep drawing around them. ISIG stays on so
// ctrl+c still interrupts.
func quietTerminal(input *os.File) func() {
	fd := int(input.Fd())
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return func() {}
	}
	quiet := *saved
	quiet.Lflag &^= unix.ECHO | unix.ICANON
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return func() {}
	}
	return func() {
		// The flushing variant drops replies that arrived after the last frame,
		// so the shell does not read them as typed input.
		_ = unix.IoctlSetTermios(fd, ioctlSetTermiosFlush, saved)
	}
}
