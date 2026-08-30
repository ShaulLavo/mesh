package dnsname

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

const renewalLockPollInterval = 25 * time.Millisecond

type renewalLock struct {
	file *os.File
}

func acquireRenewalLock(ctx context.Context, path string) (*renewalLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("dnsname: wait for renewal lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is a fixed private-state lock and the opened descriptor is verified below
	if err != nil {
		return nil, fmt.Errorf("dnsname: open renewal lock: %w", err)
	}
	closeOnError := func(cause error) (*renewalLock, error) {
		return nil, errors.Join(cause, file.Close())
	}

	pathInfo, err := os.Lstat(path)
	if err != nil {
		return closeOnError(fmt.Errorf("dnsname: inspect renewal lock: %w", err))
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("dnsname: inspect open renewal lock: %w", err))
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
		return closeOnError(errors.New("dnsname: renewal lock is not a regular file"))
	}
	if err := file.Chmod(0o600); err != nil {
		return closeOnError(fmt.Errorf("dnsname: secure renewal lock: %w", err))
	}

	for {
		if err := ctx.Err(); err != nil {
			return closeOnError(fmt.Errorf("dnsname: wait for renewal lock: %w", err))
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &renewalLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return closeOnError(fmt.Errorf("dnsname: acquire renewal lock: %w", err))
		}

		timer := time.NewTimer(renewalLockPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return closeOnError(fmt.Errorf("dnsname: wait for renewal lock: %w", ctx.Err()))
		case <-timer.C:
		}
	}
}

func (l *renewalLock) release() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}
