package worker

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/x/xpty"
	"golang.org/x/sys/unix"
)

type workerPTY struct {
	*xpty.UnixPty
	master *os.File
}

func newWorkerPTY(cols, rows int) (xpty.Pty, error) {
	pty, err := xpty.NewUnixPty(cols, rows)
	if err != nil {
		return nil, workerExecutionError("open PTY", err)
	}
	master, err := pollablePTYMaster(pty)
	if err != nil {
		_ = pty.Close()
		return nil, fmt.Errorf("prepare PTY polling: %w", err)
	}
	return &workerPTY{UnixPty: pty, master: master}, nil
}

func pollablePTYMaster(pty *xpty.UnixPty) (*os.File, error) {
	fd := -1
	var dupErr error
	err := pty.Control(func(source uintptr) {
		fd, dupErr = unix.FcntlInt(source, unix.F_DUPFD_CLOEXEC, 0)
	})
	if err != nil || dupErr != nil {
		return nil, workerExecutionError("duplicate PTY master", errors.Join(err, dupErr))
	}
	// creack/pty opens the master in blocking mode, including Go's internal
	// File state. A fresh File over a nonblocking duplicate joins the runtime
	// poller on Linux and Darwin, so Close can interrupt reads and writes.
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, workerExecutionError("set PTY nonblocking", err)
	}
	master := os.NewFile(uintptr(fd), pty.Name())
	if err := master.SetDeadline(time.Time{}); err != nil {
		_ = master.Close()
		return nil, workerExecutionError("register PTY poller", err)
	}
	return master, nil
}

func (p *workerPTY) Read(data []byte) (int, error) {
	n, err := p.master.Read(data)
	return n, workerExecutionError("read PTY", err)
}

func (p *workerPTY) Write(data []byte) (int, error) {
	n, err := p.master.Write(data)
	return n, workerExecutionError("write PTY", err)
}

// NewFile preserves an already-nonblocking descriptor when Fd is called.
// Observation and foreground checks must not turn the pump's reads blocking.
func (p *workerPTY) Fd() uintptr { return p.master.Fd() }

func (p *workerPTY) Close() error {
	return errors.Join(p.master.Close(), p.UnixPty.Close())
}
