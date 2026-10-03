package updateinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const helperMetadataLimit = 64 << 10

type helperMetadataCapture struct {
	data     []byte
	cancel   context.CancelFunc
	overflow bool
}

func (c *helperMetadataCapture) Write(data []byte) (int, error) {
	if len(data) > helperMetadataLimit-len(c.data) {
		c.overflow = true
		c.cancel()
		return 0, errors.New("helper build report exceeds size limit")
	}
	c.data = append(c.data, data...)
	return len(data), nil
}

func helperMetadataOutput(ctx context.Context, executable string) ([]byte, error) {
	probe, cancel := context.WithCancel(ctx)
	defer cancel()
	capture := &helperMetadataCapture{data: make([]byte, 0, helperMetadataLimit), cancel: cancel}
	command := exec.CommandContext(probe, executable, "version", "--json") //nolint:gosec // verified local helper image
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 50 * time.Millisecond
	command.Stdout, command.Stderr = capture, capture
	command.Cancel = func() error {
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		if err != nil {
			return fmt.Errorf("stop helper build probe: %w", err)
		}
		return nil
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start helper build probe: %w", err)
	}
	defer func() { _ = unix.Kill(-command.Process.Pid, unix.SIGKILL) }()
	err := command.Wait()
	if capture.overflow {
		return nil, errors.New("helper build report exceeds size limit")
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("helper build probe: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("helper build probe: %w", err)
	}
	return capture.data, nil
}
