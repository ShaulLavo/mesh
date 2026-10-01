//go:build !windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
	pollFD, err := attachmentPollFD(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), output.Name())
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("make terminal output cancellation pipe: %w", err)
	}
	cancelFD, err := attachmentPollFD(int(cancelRead.Fd()))
	if err != nil {
		_ = cancelRead.Close()
		_ = cancelWrite.Close()
		_ = file.Close()
		return nil, nil, err
	}
	writer := &attachmentFileOutput{file: file, ctx: ctx, fd: fd, pollFD: pollFD, cancelFD: cancelFD}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		// EOF wakes poll without writing into a potentially full wake-up pipe.
		_ = cancelWrite.Close()
	})
	return writer, func() {
		if !stop() {
			<-done
		}
		_ = cancelWrite.Close()
		_ = cancelRead.Close()
		_ = file.Close()
	}, nil
}

func attachmentPollFD(fd int) (int32, error) {
	if fd < 0 || fd > math.MaxInt32 {
		return 0, fmt.Errorf("terminal output descriptor %d exceeds poll range", fd)
	}
	return int32(fd), nil
}

func (o *attachmentFileOutput) write(payload []byte, deadline time.Time) (int, error) {
	written := 0
	for written < len(payload) {
		if err := o.waitWritable(deadline); err != nil {
			return written, err
		}
		// Stay within POSIX's minimum PIPE_BUF without changing flags shared
		// with stderr, background jobs, or the user's parent shell. Readiness
		// is a snapshot, not a capacity reservation against other writers.
		end := min(written+512, len(payload))
		n, err := unix.Write(o.fd, payload[written:end])
		if n > 0 {
			written += n
		}
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return written, fmt.Errorf("write terminal output: %w", err)
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (o *attachmentFileOutput) waitWritable(deadline time.Time) error {
	for {
		if err := o.ctx.Err(); err != nil {
			return fmt.Errorf("write terminal output: %w", err)
		}
		timeout := -1
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return os.ErrDeadlineExceeded
			}
			timeout = int(remaining/time.Millisecond) + 1
		}
		poll := []unix.PollFd{
			{Fd: o.pollFD, Events: unix.POLLOUT},
			{Fd: o.cancelFD, Events: unix.POLLIN},
		}
		ready, err := unix.Poll(poll, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll terminal output: %w", err)
		}
		switch {
		case poll[1].Revents != 0:
			return fmt.Errorf("write terminal output: %w", o.ctx.Err())
		case ready == 0:
			return os.ErrDeadlineExceeded
		case poll[0].Revents&unix.POLLNVAL != 0:
			return os.ErrClosed
		case poll[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0:
			return io.ErrClosedPipe
		case poll[0].Revents&unix.POLLOUT != 0:
			return nil
		}
	}
}
