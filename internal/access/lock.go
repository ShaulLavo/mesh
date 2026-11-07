package access

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// WithClassWriter acquires class exclusion before any native role lock. The
// callback receives the original absolute admission deadline, not a new budget.
func WithClassWriter(ctx context.Context, stateDir string, budget time.Duration, change func(context.Context) error) error {
	wait, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := wait.Err(); err != nil {
		return fmt.Errorf("access: class writer cancelled: %w", err)
	}
	dir, err := scopeDirectory(stateDir, true)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	file, err := writerLock(wait, dir, "key-classes.lock")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return change(wait)
}
func writerLock(ctx context.Context, dir *os.File, name string) (*os.File, error) {
	// Concurrent nonexclusive creation can fail with ENOENT on Darwin.
	file, _, err := privateFile(dir, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if errors.Is(err, os.ErrExist) {
		file, _, err = privateFile(dir, name, unix.O_RDWR)
	}
	if err != nil {
		return nil, err
	}
	if err := WaitWriter(ctx, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// WaitWriter is shared by the existing native publishers. Only writers call it.
func WaitWriter(ctx context.Context, file *os.File) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var contention error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("access: writer admission: %w", errors.Join(err, contention))
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("access: lock writer: %w", err)
		}
		contention = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("access: writer admission: %w", errors.Join(ctx.Err(), contention))
		case <-ticker.C:
		}
	}
}
