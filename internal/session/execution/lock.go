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

var lockWaitLimit = 1500 * time.Millisecond

// Close releases the fence. A nil fence stands for a lock file an earlier
// boot lost, which there is nothing to release for.
func (g *Fence) Close() error {
	if g == nil {
		return nil
	}
	return executionBoundaryError("lock.go Close", g.file.Close())
}
func (f *Files) Lock(ctx context.Context, name string) (*Fence, error) {
	return f.lockFile(ctx, name, os.O_CREATE|os.O_RDWR, unix.LOCK_EX)
}
func (f *Files) LockExisting(ctx context.Context, name string) (*Fence, error) {
	return f.lockFile(ctx, name, os.O_RDWR, unix.LOCK_EX)
}

type callerWaitKey struct{}

// WithCallerWait lets an output read under ctx wait for its stream fences up
// to ctx's own deadline instead of lockWaitLimit. A read whose caller said how
// long it will wait uses it, so a loaded host answers within that budget
// rather than failing after 1.5 s. Other locks, such as the journal's, keep
// their own limit. Without a deadline on ctx the short default still applies.
func WithCallerWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, callerWaitKey{}, true)
}

// streamReadWait is the budget for every stream fence one read takes.
func streamReadWait(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok && ctx.Value(callerWaitKey{}) == true {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, lockWaitLimit)
}

func (f *Files) lockFile(ctx context.Context, name string, flags, mode int) (*Fence, error) {
	ctx, cancel := context.WithTimeout(ctx, lockWaitLimit)
	defer cancel()
	return f.lockWithin(ctx, name, flags, mode)
}

// lockWithin waits for the lock until ctx ends, with no limit of its own.
func (f *Files) lockWithin(ctx context.Context, name string, flags, mode int) (*Fence, error) {
	file, err := f.open(name, flags)
	if err != nil {
		return nil, err
	}
	if err := f.waitLock(ctx, file, name, mode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Fence{file: file}, nil
}
func (f *Files) waitLock(ctx context.Context, file *os.File, name string, mode int) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return executionBoundaryError("lock.go waitLock", err)
		}
		err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
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
