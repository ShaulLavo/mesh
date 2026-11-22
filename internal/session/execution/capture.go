package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

type Capture struct {
	publishedObserved uint64
	publishedLoss     string
	store             *streamStore
	writeCtx          context.Context
	cancelWrite       context.CancelFunc
	mu                sync.Mutex
	queue             []byte
	head, size        int
	start, observed   uint64
	loss              string
	closed            bool
	reason            string
	wake              chan struct{}
	space             chan struct{}
	done              chan struct{}
	summary           StreamResult
	err               error
}

func OpenCapture(state string, b Binding, stream string) (*Capture, error) {
	if err := b.Validate(); err != nil {
		return nil, executionBoundaryError("capture", err)
	}
	if !slices.Contains(b.Streams(), stream) {
		return nil, fmt.Errorf("execution: invalid capture stream")
	}
	f, err := OpenFiles(state)
	if err != nil {
		return nil, executionBoundaryError("capture", err)
	}
	s := newStreamStore(f, b, stream)
	writeCtx, cancel := context.WithCancel(context.Background())
	c := &Capture{store: s, writeCtx: writeCtx, cancelWrite: cancel, wake: make(chan struct{}, 1), space: make(chan struct{}, 1), done: make(chan struct{})}
	if err := c.initialize(); err != nil {
		cancel()
		return nil, errors.Join(err, f.Close())
	}
	go c.writeLoop()
	return c, nil
}
func (c *Capture) initialize() error {
	s := c.store
	if err := s.files.Ensure(s.path(".")); err != nil {
		return executionBoundaryError("capture", err)
	}
	gate, err := s.files.Lock(context.Background(), s.path("stream.lock"))
	if err != nil {
		return executionBoundaryError("capture", err)
	}
	defer func() { _ = gate.Close() }()
	if err := s.files.Confirm(s.path("stream.lock")); err != nil {
		return executionBoundaryError("capture", err)
	}
	if err := s.createReaderLock(); err != nil {
		return err
	}
	generation, err := freshToken()
	if err != nil {
		return executionBoundaryError("capture", err)
	}
	m := manifest{Binding: s.binding, Generation: generation, Segments: []segment{}}
	if s.stream != KindPTY {
		m.Stream = s.stream
	}
	if _, err := s.files.Publish(s.path("manifest.json"), m, true); err != nil {
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
	c.enqueue(data, offset)
}

// OfferExact waits for the bounded disk queue until the reader is stopped.
func (c *Capture) OfferExact(ctx context.Context, data []byte) bool {
	for chunk := range slices.Chunk(data, captureQueue) {
		if !c.offerExactChunk(ctx, chunk) {
			return false
		}
	}
	return true
}

func (c *Capture) offerExactChunk(ctx context.Context, data []byte) bool {
	for {
		c.mu.Lock()
		if c.closed || ctx.Err() != nil || c.writeCtx.Err() != nil {
			c.mu.Unlock()
			return false
		}
		if c.size+len(data) <= captureQueue {
			offset := c.observed
			c.observed += uint64(len(data))
			c.enqueue(data, offset)
			c.mu.Unlock()
			return true
		}
		c.notify()
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-c.writeCtx.Done():
			return false
		case <-c.space:
		}
	}
}

// Abort stops future capture commits. An in-progress filesystem operation
// remains fenced by the stream lock until that operation returns.
func (c *Capture) Abort() { c.cancelWrite() }

func (c *Capture) enqueue(data []byte, offset uint64) {
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
	if c.size >= int(segmentCapacity(c.store.binding)) {
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
	batchSize := captureSegment
	if c.store.binding.Mode() != KindPTY {
		batchSize = captureQueue
	}
	n := min(c.size, batchSize)
	b := captureBatch{data: make([]byte, n), start: c.start, observed: c.observed, loss: c.loss, final: c.closed && n == c.size, reason: c.reason}
	if n == 0 {
		return b
	}
	read := copy(b.data, c.queue[c.head:min(len(c.queue), c.head+n)])
	copy(b.data[read:], c.queue[:n-read])
	c.head = (c.head + n) % len(c.queue)
	c.size -= n
	c.start += uint64(len(b.data))
	select {
	case c.space <- struct{}{}:
	default:
	}
	return b
}
func (c *Capture) writeLoop() {
	defer close(c.done)
	defer c.cancelWrite()
	defer func() { c.err = errors.Join(c.err, c.store.files.Close()) }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.writeCtx.Done():
			return
		case <-ticker.C:
		case <-c.wake:
		}
		if c.flush() {
			return
		}
	}
}
func (c *Capture) flush() bool {
	for c.writeCtx.Err() == nil {
		b := c.take()
		if len(b.data) == 0 && !b.final && b.observed == c.publishedObserved && b.loss == c.publishedLoss {
			return false
		}
		summary, err := c.commit(b)
		c.err = err
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
		full := c.size >= int(segmentCapacity(c.store.binding))
		c.mu.Unlock()
		if !full {
			return false
		}
	}
	return true
}
func (c *Capture) storageLoss() { c.mu.Lock(); c.loss = captureStorageLoss; c.mu.Unlock() }
func (c *Capture) commit(b captureBatch) (StreamResult, error) {
	s := c.store
	ctx := c.writeCtx
	if ctx == nil {
		ctx = context.Background()
	}
	gate, err := s.writerTurn(ctx, !b.final)
	if err != nil {
		return StreamResult{}, err
	}
	defer func() { _ = gate.Close() }()
	m, err := s.load()
	if err != nil {
		return StreamResult{}, err
	}
	if err := s.settlePrevious(ctx, m); err != nil {
		return StreamResult{}, err
	}
	if m.Closed {
		return StreamResult{}, fmt.Errorf("execution: capture already closed")
	}
	if err := c.appendBatch(&m, b); err != nil {
		return StreamResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return StreamResult{}, executionBoundaryError("publish capture", err)
	}
	name := s.path("manifest.json")
	if _, err := s.files.Publish(name, m, false); err != nil {
		return StreamResult{}, err
	}
	s.rememberPublished(name)
	if err := s.prune(ctx, m); err != nil {
		return StreamResult{}, err
	}
	return m.summary(), nil
}
func (s *streamStore) settlePrevious(ctx context.Context, m manifest) error {
	if s.binding.Mode() != KindPTY && s.confirmedManifest(m) {
		return nil
	}
	return s.prune(ctx, m)
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
	capacity := int(segmentCapacity(c.store.binding))
	if len(m.Segments) > 0 && int(m.Segments[len(m.Segments)-1].Size) < capacity {
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
		n := min(len(data), capacity)
		seg, err := c.writeSegment(start, data[:n])
		if err != nil {
			return executionBoundaryError("capture", err)
		}
		m.Segments = append(m.Segments, seg)
		start += uint64(len(data[:n]))
		data = data[n:]
	}
	m.End = start
	c.trimSegments(m)
	return nil
}

func (c *Capture) trimSegments(m *manifest) {
	limit := retentionBytes(c.store.binding)
	if m.End-m.Earliest <= limit {
		return
	}
	if c.store.binding.Mode() == KindPTY {
		c.trimPTYSuffix(m, limit)
	} else {
		m.Earliest = m.End - limit
		for m.Segments[0].Start+uint64(m.Segments[0].Size) <= m.Earliest {
			m.Segments = m.Segments[1:]
		}
	}
	if m.Loss == "" {
		m.Loss = captureRetention
	}
}

func (c *Capture) trimPTYSuffix(m *manifest, limit uint64) {
	for m.End-m.Earliest > limit {
		first := m.Segments[0]
		m.Segments = m.Segments[1:]
		m.Earliest = first.Start + uint64(first.Size)
	}
}

func (c *Capture) writeSegment(start uint64, data []byte) (segment, error) {
	size := len(data)
	if size < 1 || size > int(segmentCapacity(c.store.binding)) {
		return segment{}, fmt.Errorf("execution: invalid segment length")
	}
	name, err := freshToken()
	if err != nil {
		return segment{}, err
	}
	sum := sha256.Sum256(data)
	seg := segment{Name: name, Start: start, Size: uint32(size), Hash: hex.EncodeToString(sum[:])} //nolint:gosec // the segment capacity bounds size
	path := c.store.path(name)
	_, err = c.store.files.publishBytes(path, data, true)
	if err == nil {
		c.store.rememberPublished(path)
	}
	return seg, err
}
