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
	dst      Conn
	opts     BatchOptions
	mu       sync.Mutex
	pending  *protocol.Frame
	timer    *time.Timer
	writeErr error
	closed   bool
	closeErr error
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
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClosed
	}
	if c.writeErr != nil {
		return c.writeErr
	}
	if frame.Kind != protocol.KindData || len(frame.Payload) == 0 {
		if err := c.flushLocked(); err != nil {
			return err
		}
		return c.writeLocked(frame)
	}
	if len(frame.Payload) >= c.opts.MaxBytes {
		if err := c.flushLocked(); err != nil {
			return err
		}
		return c.writeLocked(frame)
	}

	if c.pending != nil && !contiguous(*c.pending, frame, c.opts.MaxBytes) {
		if err := c.flushLocked(); err != nil {
			return err
		}
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
		return c.flushLocked()
	}
	return nil
}

// Flush writes pending output now.
func (c *BatchingConn) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.closeErr
	}
	return c.flushLocked()
}

func (c *BatchingConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	flushErr := c.flushLocked()
	closeErr := c.dst.Close()
	c.closeErr = errors.Join(flushErr, closeErr)
	return c.closeErr
}

func (c *BatchingConn) flushLocked() error {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.writeErr != nil {
		return c.writeErr
	}
	if c.pending == nil {
		return nil
	}
	frame := *c.pending
	c.pending = nil
	return c.writeLocked(frame)
}

func (c *BatchingConn) writeLocked(frame protocol.Frame) error {
	if err := c.dst.WriteFrame(frame); err != nil {
		c.writeErr = err
		return err
	}
	return nil
}

func (c *BatchingConn) scheduleFlushLocked() {
	if c.opts.FlushInterval == 0 {
		return
	}
	c.timer = time.AfterFunc(c.opts.FlushInterval, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.timer = nil
		if c.closed || c.writeErr != nil {
			return
		}
		_ = c.flushLocked()
	})
}

func contiguous(left, right protocol.Frame, maxBytes int) bool {
	if left.Session != right.Session || len(left.Payload)+len(right.Payload) > maxBytes {
		return false
	}
	next := left.Seq + uint64(len(left.Payload))
	return next >= left.Seq && next == right.Seq
}

var _ Conn = (*BatchingConn)(nil)
