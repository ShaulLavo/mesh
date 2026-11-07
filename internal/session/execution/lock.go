package execution

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"time"
)

type Fence struct{ file *os.File }

func (g *Fence) Close() error { return executionBoundaryError("lock.go Close", g.file.Close()) }
func (f *Files) Lock(ctx context.Context, name string) (*Fence, error) {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	file, err := f.open(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := f.waitLock(ctx, file, name); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Fence{file: file}, nil
}
func (f *Files) waitLock(ctx context.Context, file *os.File, name string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return executionBoundaryError("lock.go waitLock", err)
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f.currentLock(file, name)
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return executionBoundaryError("lock.go waitLock", err)
		}
		select {
		case <-ctx.Done():
			return executionBoundaryError("lock.go waitLock", ctx.Err())
		case <-ticker.C:
		}
	}
}
func (f *Files) currentLock(file *os.File, name string) error {
	opened, err := file.Stat()
	if err != nil {
		return executionBoundaryError("lock.go currentLock", err)
	}
	current, err := f.root.Lstat(name)
	if err != nil {
		return executionBoundaryError("lock.go currentLock", err)
	}
	if !os.SameFile(opened, current) {
		return fmt.Errorf("execution: replaced lock inode")
	}
	return regular(file)
}
