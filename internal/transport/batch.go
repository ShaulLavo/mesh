package transport

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

const (
	defaultBatchBytes    = 32 << 10
	defaultBatchInterval = 2 * time.Millisecond
	dataFrameOverhead    = 16
)

// BatchOptions controls data-frame coalescing.
type BatchOptions struct {
	MaxBytes      int
	FlushInterval time.Duration
}

// BatchingConn coalesces adjacent data frames for one session. Control and
// input frames flush pending output first, so wire ordering does not change.
type BatchingConn struct {
	dst       Conn
	opts      BatchOptions
	stateMu   sync.Mutex
	writeMu   sync.Mutex
	pending   *protocol.Frame
	timer     *time.Timer
	timerGen  uint64
	writeErr  error
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// NewBatchingConn wraps dst with bounded, timer-driven data coalescing.
func NewBatchingConn(dst Conn, opts BatchOptions) (*BatchingConn, error) {
	if dst == nil {
		return nil, fmt.Errorf("transport: nil batching destination")
	}
	var err error
	opts, err = normalizeBatchOptions(opts)
	if err != nil {
		return nil, err
	}
	return &BatchingConn{dst: dst, opts: opts}, nil
}

func normalizeBatchOptions(opts BatchOptions) (BatchOptions, error) {
	if opts.MaxBytes == 0 {
		opts.MaxBytes = defaultBatchBytes
	}
	if opts.FlushInterval == 0 {
		opts.FlushInterval = defaultBatchInterval
	}
	if opts.MaxBytes < 1 || opts.MaxBytes > protocol.MaxPayload-dataFrameOverhead {
		return BatchOptions{}, fmt.Errorf(
			"transport: batch size must be between 1 and %d bytes",
			protocol.MaxPayload-dataFrameOverhead,
		)
	}
	if opts.FlushInterval < 0 {
		return BatchOptions{}, fmt.Errorf("transport: batch flush interval must not be negative")
	}
	return opts, nil
}

func (c *BatchingConn) ReadFrame() (protocol.Frame, error) {
	return c.dst.ReadFrame()
}

// WriteFrame copies buffered data before returning. A later Flush, control
// write, or Close reports an asynchronous data-write failure.
func (c *BatchingConn) WriteFrame(frame protocol.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return ErrClosed
	}
	if c.writeErr != nil {
		err := c.writeErr
		c.stateMu.Unlock()
		return err
	}
	frames := c.queueLocked(frame)
	c.stateMu.Unlock()
	return c.writeFrames(frames)
}

// Flush writes pending output now.
func (c *BatchingConn) Flush() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return ErrClosed
	}
	if c.writeErr != nil {
		err := c.writeErr
		c.stateMu.Unlock()
		return err
	}
	frame, ok := c.takePendingLocked()
	c.stateMu.Unlock()
	if !ok {
		return nil
	}
	return c.writeFrames([]protocol.Frame{frame})
}

// Close cancels queued output and interrupts active operations. Call Flush
// first when queued output must be delivered before shutdown.
func (c *BatchingConn) Close() error {
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		c.closed = true
		c.cancelTimerLocked()
		c.pending = nil
		c.stateMu.Unlock()

		// Conn.Close must interrupt an in-flight WriteFrame. Do not wait for
		// writeMu before giving the destination that cancellation signal.
		destinationErr := c.dst.Close()
		c.writeMu.Lock()
		c.writeMu.Unlock() //nolint:staticcheck // lock and unlock form a completion barrier for an in-flight write

		c.stateMu.Lock()
		c.closeErr = errors.Join(c.writeErr, destinationErr)
		c.stateMu.Unlock()
	})
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.closeErr
}

func (c *BatchingConn) queueLocked(frame protocol.Frame) []protocol.Frame {
	if frame.Kind != protocol.KindData || len(frame.Payload) == 0 || len(frame.Payload) >= c.opts.MaxBytes {
		frames := c.takePendingFramesLocked()
		return append(frames, frame)
	}

	var frames []protocol.Frame
	if c.pending != nil && !contiguous(*c.pending, frame, c.opts.MaxBytes) {
		frames = c.takePendingFramesLocked()
	}
	if c.pending == nil {
		copy := frame
		copy.Payload = bytes.Clone(frame.Payload)
		c.pending = &copy
		c.scheduleFlushLocked()
	} else {
		c.pending.Payload = append(c.pending.Payload, frame.Payload...)
	}
	if len(c.pending.Payload) >= c.opts.MaxBytes {
		frames = append(frames, c.takePendingFramesLocked()...)
	}
	return frames
}

func (c *BatchingConn) takePendingFramesLocked() []protocol.Frame {
	frame, ok := c.takePendingLocked()
	if !ok {
		return nil
	}
	return []protocol.Frame{frame}
}

func (c *BatchingConn) takePendingLocked() (protocol.Frame, bool) {
	c.cancelTimerLocked()
	if c.pending == nil {
		return protocol.Frame{}, false
	}
	frame := *c.pending
	c.pending = nil
	return frame, true
}

func (c *BatchingConn) writeFrames(frames []protocol.Frame) error {
	for _, frame := range frames {
		if err := c.dst.WriteFrame(frame); err != nil {
			c.stateMu.Lock()
			if c.writeErr == nil {
				c.writeErr = err
			}
			c.stateMu.Unlock()
			return err
		}
	}
	return nil
}

func (c *BatchingConn) scheduleFlushLocked() {
	if c.opts.FlushInterval == 0 {
		return
	}
	c.timerGen++
	generation := c.timerGen
	c.timer = time.AfterFunc(c.opts.FlushInterval, func() {
		c.flushTimer(generation)
	})
}

func (c *BatchingConn) flushTimer(generation uint64) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.stateMu.Lock()
	if generation != c.timerGen {
		c.stateMu.Unlock()
		return
	}
	c.timer = nil
	if c.closed || c.writeErr != nil || c.pending == nil {
		c.stateMu.Unlock()
		return
	}
	frame := *c.pending
	c.pending = nil
	c.stateMu.Unlock()
	_ = c.writeFrames([]protocol.Frame{frame})
}

func (c *BatchingConn) cancelTimerLocked() {
	c.timerGen++
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

func contiguous(left, right protocol.Frame, maxBytes int) bool {
	if left.Session != right.Session || len(left.Payload)+len(right.Payload) > maxBytes {
		return false
	}
	next := left.Seq + uint64(len(left.Payload))
	return next >= left.Seq && next == right.Seq
}

var _ Conn = (*BatchingConn)(nil)
