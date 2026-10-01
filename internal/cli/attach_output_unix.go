//go:build !linux && !windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func localAttachOutput(ctx context.Context, output *os.File) (*attachmentFileOutput, func(), error) {
	raw, err := output.SyscallConn()
	if err != nil {
		return nil, nil, fmt.Errorf("access terminal output: %w", err)
	}
	fd := -1
	var duplicateErr error
	err = raw.Control(func(source uintptr) {
		fd, duplicateErr = unix.FcntlInt(source, unix.F_DUPFD_CLOEXEC, 0)
	})
	if err != nil || duplicateErr != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return nil, nil, fmt.Errorf("duplicate terminal output: %w", errors.Join(err, duplicateErr))
	}
	// Darwin poll cannot handle tty devices. Keep the existing blocking
	// behavior until a cancellation mechanism has been runtime-tested there.
	file := os.NewFile(uintptr(fd), output.Name())
	writer := &attachmentFileOutput{file: file, ctx: ctx}
	return writer, func() { _ = file.Close() }, nil
}

func (o *attachmentFileOutput) write(payload []byte, _ time.Time) (int, error) {
	if err := o.ctx.Err(); err != nil {
		return 0, fmt.Errorf("write terminal output: %w", err)
	}
	n, err := o.file.Write(payload)
	if err != nil {
		return n, fmt.Errorf("write terminal output: %w", err)
	}
	return n, nil
}
