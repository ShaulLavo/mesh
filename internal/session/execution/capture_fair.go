package execution

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Readers announce themselves on reader.lock before they wait for
// stream.lock, and the capture writer steps aside for announced readers
// between commits. Without the hand-off, a writer whose queue stays full
// retakes stream.lock microseconds after each commit, and a reader that polls
// every 10 ms almost never lands in that gap: on a loaded host a tailing
// reader got nothing until the writer went idle (#318).
const readerLockName = "reader.lock"

// readerYieldLimit bounds how long one commit waits for announced readers,
// so a reader stuck behind another holder of stream.lock cannot stall capture.
var readerYieldLimit = time.Second

const readerYieldPoll = time.Millisecond

func (s *streamStore) createReaderLock() error {
	name := s.path(readerLockName)
	file, err := s.files.open(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return executionBoundaryError("create capture reader lock", err)
	}
	if err := s.files.closeFile(file); err != nil {
		return executionBoundaryError("create capture reader lock", err)
	}
	return executionBoundaryError("confirm capture reader lock", s.files.Confirm(name))
}

// readerFence takes stream.lock in mode while announced on reader.lock. The
// announcement ends once stream.lock is held, so the writer resumes as soon
// as this read releases it.
func (s *streamStore) readerFence(ctx context.Context, mode int) (*Fence, error) {
	intent, err := s.files.lockFile(ctx, s.path(readerLockName), os.O_RDWR, unix.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer func() { _ = intent.Close() }()
	return s.files.lockFile(ctx, s.path("stream.lock"), os.O_RDWR, mode)
}

// yieldUnlessFinal steps aside for readers before an ordinary commit. The
// final commit runs under the worker's exit budget and closes the stream,
// which is what waiting readers need next, so it goes first.
func (c *Capture) yieldUnlessFinal(ctx context.Context, b captureBatch) error {
	if b.final {
		return nil
	}
	return c.store.yieldToReaders(ctx)
}

// yieldToReaders waits until no reader is announced, or readerYieldLimit.
// An announced reader holds reader.lock shared, so an exclusive probe
// succeeds only when none is waiting; the probe is released at once.
func (s *streamStore) yieldToReaders(ctx context.Context) error {
	file, err := s.files.open(s.path(readerLockName), os.O_RDWR)
	if err != nil {
		return executionBoundaryError("yield capture to readers", err)
	}
	defer func() { _ = file.Close() }()
	ctx, cancel := context.WithTimeout(ctx, readerYieldLimit)
	defer cancel()
	ticker := time.NewTicker(readerYieldPoll)
	defer ticker.Stop()
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return executionBoundaryError("yield capture to readers", err)
		}
		select {
		case <-ctx.Done():
			// The yield is only for fairness; the commit itself decides what a
			// cancelled writer does.
			return nil
		case <-ticker.C:
		}
	}
}
