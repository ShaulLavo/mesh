package execution

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Readers and the capture writer take turns on stream.lock through
// reader.lock. A reader holds reader.lock shared while it waits for
// stream.lock. Before a commit the writer waits until no reader is announced,
// then holds reader.lock exclusively while it waits for stream.lock, so
// readers that arrive meanwhile queue behind the commit instead of keeping
// overlapping shared locks forever. Without the hand-off, a writer whose queue
// stays full retakes stream.lock microseconds after each commit, and a reader
// that polls every 10 ms almost never lands in that gap: on a loaded host a
// tailing reader got nothing until the writer went idle (#318).
//
// reader.lock is only coordination state. A stream without one, such as one
// written before the hand-off existed, is read as having no reader waiting.
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

// readerFence takes stream.lock in mode while announced on reader.lock. Both
// waits share one budget: the caller's own deadline under WithCallerWait,
// otherwise lockWaitLimit. The announcement ends once stream.lock is held.
func (s *streamStore) readerFence(ctx context.Context, mode int) (*Fence, error) {
	ctx, cancel := streamReadWait(ctx)
	defer cancel()
	intent, err := s.files.lockWithin(ctx, s.path(readerLockName), os.O_RDWR, unix.LOCK_SH)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if intent != nil {
		defer func() { _ = intent.Close() }()
	}
	return s.files.lockWithin(ctx, s.path("stream.lock"), os.O_RDWR, mode)
}

// writerTurn takes stream.lock for one commit. An ordinary commit first lets
// announced readers in; the final commit closes the stream, which is what
// waiting readers need next, so it goes first. The stream wait has no default
// limit: a reader holds the stream for one bounded page, and giving up would
// discard the batch the writer already took from its queue. Only ctx (the
// capture's own write context) ends it.
func (s *streamStore) writerTurn(ctx context.Context, yield bool) (*Fence, error) {
	intent, err := s.files.open(s.path(readerLockName), os.O_RDWR)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, executionBoundaryError("open capture reader lock", err)
	}
	if intent != nil {
		defer func() { _ = intent.Close() }()
		if err := claimReaderLock(ctx, intent, yield); err != nil {
			return nil, err
		}
	}
	return s.files.lockWithin(ctx, s.path("stream.lock"), os.O_RDWR, unix.LOCK_EX)
}

// claimReaderLock takes reader.lock exclusively once no reader is announced,
// waiting up to readerYieldLimit when yield is set. When announced readers
// stay past that, the writer goes on without the claim and simply competes
// for stream.lock.
func claimReaderLock(ctx context.Context, intent *os.File, yield bool) error {
	limit := time.Duration(0)
	if yield {
		limit = readerYieldLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(readerYieldPoll)
	defer ticker.Stop()
	for {
		err := unix.Flock(int(intent.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return executionBoundaryError("claim capture reader lock", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
