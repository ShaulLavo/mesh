package execution

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Readers and the capture writer take turns on stream.lock. A reader holds
// reader.lock shared while it waits for stream.lock, and before an ordinary
// commit the writer waits until no reader is announced. Without that hand-off,
// a writer whose queue stays full retakes stream.lock microseconds after each
// commit, and a reader that polls every 10 ms almost never lands in that gap:
// on a loaded host a tailing reader got nothing until the writer went idle
// (#318).
//
// The writer gets its turn too. If readers keep stream.lock past
// readerYieldLimit, their shared locks overlapping, the writer holds
// writer.lock, which new readers wait on before they announce, until it has
// the stream. The writer holds no claim in the ordinary case: a claim held
// through every stream wait kept a tailing reader from announcing and cut its
// share of turns in half.
//
// Both files are only coordination state. A stream without them, such as one
// written before the hand-off existed, reads as having no one waiting.
const (
	readerLockName = "reader.lock"
	writerLockName = "writer.lock"
)

// readerYieldLimit bounds how long one commit waits for announced readers,
// and how long it waits on readers' shared locks before claiming its turn.
var readerYieldLimit = time.Second

const readerYieldPoll = time.Millisecond

func (s *streamStore) createTurnLocks() error {
	for _, name := range []string{readerLockName, writerLockName} {
		path := s.path(name)
		file, err := s.files.open(path, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return executionBoundaryError("create capture "+name, err)
		}
		if err := s.files.closeFile(file); err != nil {
			return executionBoundaryError("create capture "+name, err)
		}
		if err := s.files.Confirm(path); err != nil {
			return executionBoundaryError("confirm capture "+name, err)
		}
	}
	return nil
}

// readerFence takes stream.lock in mode while announced on reader.lock. All
// its waits share one budget: the caller's own deadline under WithCallerWait,
// otherwise lockWaitLimit. The announcement ends once stream.lock is held.
func (s *streamStore) readerFence(ctx context.Context, mode int) (*Fence, error) {
	ctx, cancel := streamReadWait(ctx)
	defer cancel()
	if err := s.waitForWriterClaim(ctx); err != nil {
		return nil, err
	}
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
// limit: giving up would discard the batch the writer already took from its
// queue. Only ctx (the capture's own write context) ends it.
func (s *streamStore) writerTurn(ctx context.Context, yield bool) (*Fence, error) {
	if yield {
		if err := s.yieldToReaders(ctx); err != nil {
			return nil, err
		}
	}
	turn, cancel := context.WithTimeout(ctx, readerYieldLimit)
	gate, err := s.files.lockWithin(turn, s.path("stream.lock"), os.O_RDWR, unix.LOCK_EX)
	cancel()
	if err == nil || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
		return gate, err
	}
	return s.claimWriterTurn(ctx)
}

// claimWriterTurn is for readers whose shared locks keep overlapping, so the
// stream is never free. Holding writer.lock stops new readers from starting
// until this commit has the stream; the readers already in finish their
// bounded pages.
func (s *streamStore) claimWriterTurn(ctx context.Context) (*Fence, error) {
	claim, err := s.files.lockWithin(ctx, s.path(writerLockName), os.O_RDWR, unix.LOCK_EX)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if claim != nil {
		defer func() { _ = claim.Close() }()
	}
	return s.files.lockWithin(ctx, s.path("stream.lock"), os.O_RDWR, unix.LOCK_EX)
}

// yieldToReaders waits until no reader is announced, or readerYieldLimit. An
// announced reader holds reader.lock shared, so an exclusive probe succeeds
// only when none is waiting; the probe is released at once, so readers can
// announce again while this commit runs.
func (s *streamStore) yieldToReaders(ctx context.Context) error {
	intent, err := s.files.open(s.path(readerLockName), os.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return executionBoundaryError("open capture reader lock", err)
	}
	defer func() { _ = intent.Close() }()
	ctx, cancel := context.WithTimeout(ctx, readerYieldLimit)
	defer cancel()
	ticker := time.NewTicker(readerYieldPoll)
	defer ticker.Stop()
	for {
		err := unix.Flock(int(intent.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return executionBoundaryError("release capture reader probe", unix.Flock(int(intent.Fd()), unix.LOCK_UN))
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return executionBoundaryError("probe capture readers", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// waitForWriterClaim returns once no writer holds writer.lock. Readers only
// probe it, so it never holds a writer back.
func (s *streamStore) waitForWriterClaim(ctx context.Context) error {
	probe, err := s.files.lockWithin(ctx, s.path(writerLockName), os.O_RDWR, unix.LOCK_SH)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return probe.Close()
}
