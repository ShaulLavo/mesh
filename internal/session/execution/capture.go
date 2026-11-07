package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Capture struct {
	publishedObserved uint64
	publishedLoss     string
	store             *streamStore
	mu                sync.Mutex
	queue             []byte
	head, size        int
	start, observed   uint64
	loss              string
	closed            bool
	reason            string
	wake              chan struct{}
	done              chan struct{}
	summary           StreamResult
	err               error
}

func OpenCapture(state string, b Binding) (*Capture, error) {
	if err := b.Validate(); err != nil {
		return nil, executionBoundaryError("capture", err)
	}
	f, err := OpenFiles(state)
	if err != nil {
		return nil, executionBoundaryError("capture", err)
	}
	s := newStreamStore(f, b)
	c := &Capture{store: s, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if err := c.initialize(); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	go c.writeLoop()
	return c, nil
}
func (c *Capture) initialize() error {
	s := c.store
	if err := s.files.Ensure(streamPath(s.binding, ".")); err != nil {
		return executionBoundaryError("capture", err)
	}
	gate, err := s.files.Lock(context.Background(), streamPath(s.binding, "stream.lock"))
	if err != nil {
		return executionBoundaryError("capture", err)
	}
	defer func() { _ = gate.Close() }()
	if err := s.files.Confirm(streamPath(s.binding, "stream.lock")); err != nil {
		return executionBoundaryError("capture", err)
	}
	generation, err := freshToken()
	if err != nil {
		return executionBoundaryError("capture", err)
	}
	m := manifest{Binding: s.binding, Generation: generation, Segments: []segment{}}
	if _, err := s.files.Publish(streamPath(s.binding, "manifest.json"), m, true); err != nil {
		return executionBoundaryError("capture", err)
	}
	return s.settle(context.Background(), m)
}

// Offer owns a bounded copy, never a filesystem operation or a consumer wait.
func (c *Capture) Offer(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(data) == 0 {
		return
	}
	offset := c.observed
	c.observed += uint64(len(data))
	if len(data) > captureQueue {
		c.loseQueue()
		return
	}
	if c.size+len(data) > captureQueue {
		c.loseQueue()
	}
	if c.queue == nil {
		c.queue = make([]byte, captureQueue)
	}
	if c.size == 0 {
		c.start = offset
	}
	tail := (c.head + c.size) % len(c.queue)
	n := copy(c.queue[tail:], data)
	copy(c.queue, data[n:])
	c.size += len(data)
	if c.size >= captureSegment {
		c.notify()
	}
}
func (c *Capture) loseQueue() {
	c.head = 0
	c.size = 0
	c.start = c.observed
	c.loss = "queue_overflow"
	c.notify()
}
func (c *Capture) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func (c *Capture) Finish(ctx context.Context, reason string) (StreamResult, error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.reason = reason
		c.notify()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return StreamResult{}, executionBoundaryError("finish capture", ctx.Err())
	case <-c.done:
		return c.summary, c.err
	}
}

type captureBatch struct {
	data            []byte
	start, observed uint64
	loss            string
	final           bool
	reason          string
}

func (c *Capture) take() captureBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := min(c.size, captureSegment)
	b := captureBatch{data: make([]byte, n), start: c.start, observed: c.observed, loss: c.loss, final: c.closed && n == c.size, reason: c.reason}
	if n == 0 {
		return b
	}
	read := copy(b.data, c.queue[c.head:min(len(c.queue), c.head+n)])
	copy(b.data[read:], c.queue[:n-read])
	c.head = (c.head + n) % len(c.queue)
	c.size -= n
	c.start += uint64(len(b.data))
	return b
}
func (c *Capture) writeLoop() {
	defer close(c.done)
	defer func() { c.err = errors.Join(c.err, c.store.files.Close()) }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-c.wake:
		}
		if c.flush() {
			return
		}
	}
}
func (c *Capture) flush() bool {
	for {
		b := c.take()
		if len(b.data) == 0 && !b.final && b.observed == c.publishedObserved && b.loss == c.publishedLoss {
			return false
		}
		summary, err := c.commit(b)
		if err == nil {
			c.publishedObserved = b.observed
			c.publishedLoss = b.loss
		}
		if err != nil {
			c.storageLoss()
			c.err = err
		}
		if b.final {
			c.summary = summary
			return true
		}
		c.mu.Lock()
		full := c.size >= captureSegment
		c.mu.Unlock()
		if !full {
			return false
		}
	}
}
func (c *Capture) storageLoss() { c.mu.Lock(); c.loss = captureStorageLoss; c.mu.Unlock() }
func (c *Capture) commit(b captureBatch) (StreamResult, error) {
	s := c.store
	ctx := context.Background()
	gate, err := s.files.LockExisting(ctx, streamPath(s.binding, "stream.lock"))
	if err != nil {
		return StreamResult{}, err
	}
	defer func() { _ = gate.Close() }()
	m, err := s.load()
	if err != nil {
		return StreamResult{}, err
	}
	if err := s.prune(ctx, m); err != nil {
		return StreamResult{}, err
	}
	if m.Closed {
		return StreamResult{}, fmt.Errorf("execution: capture already closed")
	}
	if err := c.appendBatch(&m, b); err != nil {
		return StreamResult{}, err
	}
	if _, err := s.files.Publish(streamPath(s.binding, "manifest.json"), m, false); err != nil {
		return StreamResult{}, err
	}
	if err := s.prune(ctx, m); err != nil {
		return StreamResult{}, err
	}
	return m.summary(), nil
}
func (c *Capture) appendBatch(m *manifest, b captureBatch) error {
	if len(b.data) == 0 && b.start < b.observed {
		b.start = b.observed
	}
	if b.start != m.End {
		m.Segments = nil
		m.Earliest = b.start
		m.End = b.start
		m.Loss = b.loss
		if m.Loss == "" {
			m.Loss = captureStorageLoss
		}
	}
	if b.loss != "" {
		m.Loss = b.loss
	}
	if err := c.appendData(m, b.start, b.data); err != nil {
		return executionBoundaryError("capture", err)
	}
	m.Observed = b.observed
	if b.final {
		m.Closed = true
		m.CloseReason = b.reason
		m.EOF = b.reason == captureEOF
	}
	return m.validate(c.store.binding)
}
func (c *Capture) appendData(m *manifest, start uint64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(m.Segments) > 0 && m.Segments[len(m.Segments)-1].Size < captureSegment {
		last := m.Segments[len(m.Segments)-1]
		prefix, err := c.store.segmentBytes(last)
		if err != nil {
			return executionBoundaryError("capture", err)
		}
		data = append(prefix, data...)
		start = last.Start
		m.Segments = m.Segments[:len(m.Segments)-1]
	}
	for len(data) > 0 {
		n := min(len(data), captureSegment)
		seg, err := c.writeSegment(start, data[:n])
		if err != nil {
			return executionBoundaryError("capture", err)
		}
		m.Segments = append(m.Segments, seg)
		start += uint64(len(data[:n]))
		data = data[n:]
	}
	m.End = start
	for m.End-m.Earliest > captureSuffix {
		first := m.Segments[0]
		m.Segments = m.Segments[1:]
		m.Earliest = first.Start + uint64(first.Size)
		if m.Loss == "" {
			m.Loss = captureRetention
		}
	}
	return nil
}
func (c *Capture) writeSegment(start uint64, data []byte) (segment, error) {
	size := len(data)
	if size < 1 || size > captureSegment {
		return segment{}, fmt.Errorf("execution: invalid segment length")
	}
	name, err := freshToken()
	if err != nil {
		return segment{}, err
	}
	sum := sha256.Sum256(data)
	seg := segment{Name: name, Start: start, Size: uint32(size), Hash: hex.EncodeToString(sum[:])}
	_, err = c.store.files.publishBytes(streamPath(c.store.binding, name), data, true)
	return seg, err
}
