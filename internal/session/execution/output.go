package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"golang.org/x/sys/unix"
)

// Output retains confirmations only for this reader lifetime and anchored file identities.
type Output struct {
	files   *Files
	mu      sync.Mutex
	streams map[streamKey]*streamStore
}

type streamKey struct {
	binding Binding
	name    string
}

func OpenOutput(state string) (*Output, error) {
	f, err := OpenFiles(state)
	if err != nil {
		return nil, err
	}
	return &Output{files: f, streams: make(map[streamKey]*streamStore)}, nil
}
func (o *Output) Close() error { return o.files.Close() }
func (o *Output) stream(b Binding, stream string) (*streamStore, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	key := streamKey{b, stream}
	s := o.streams[key]
	if s == nil {
		if len(o.streams) >= 128 {
			clear(o.streams)
		}
		s = newStreamStore(o.files, b, stream)
		o.streams[key] = s
	}
	return s, nil
}
func (o *Output) Page(ctx context.Context, b Binding, stream, value string, size int) (Page, error) {
	size, err := validatePageSize(size)
	if err != nil {
		return Page{}, err
	}
	if !slices.Contains(b.Streams(), stream) {
		return Page{}, fmt.Errorf("execution: stream unavailable for this kind")
	}
	s, err := o.stream(b, stream)
	if err != nil {
		return Page{}, err
	}
	if err := s.enter(ctx); err != nil {
		return Page{}, err
	}
	defer s.leave()
	return s.readPage(ctx, value, size)
}

// Tail reads up to size bytes from the retained end of stream directly, so
// small limits always return the newest captured bytes rather than paging from
// the oldest retained offset.
func (o *Output) Tail(ctx context.Context, b Binding, stream string, size int) ([]byte, error) {
	size, err := validatePageSize(size)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(b.Streams(), stream) {
		return nil, fmt.Errorf("execution: stream unavailable for this kind")
	}
	s, err := o.stream(b, stream)
	if err != nil {
		return nil, err
	}
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	defer s.leave()
	return s.readTail(ctx, size)
}

func (s *streamStore) readTail(ctx context.Context, size int) ([]byte, error) {
	gate, err := s.readerFence(ctx, unix.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gate.Close() }()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	if s.binding.Mode() != KindPTY {
		identity, err := s.confirmManifest(s.path("manifest.json"))
		if err != nil {
			return nil, err
		}
		s.confirmed[s.path("manifest.json")] = identity
	}
	if m.Expired || m.End <= m.Earliest {
		return nil, nil
	}
	start := m.Earliest
	limit := uint64(size) //nolint:gosec // validatePageSize bounds size to [1, MaxPage]
	if span := m.End - m.Earliest; span > limit {
		start = m.End - limit
	}
	tailLen := int(m.End - start) //nolint:gosec // bounded by limit <= MaxPage
	out := make([]byte, 0, tailLen)
	for _, seg := range m.Segments {
		if seg.Start+uint64(seg.Size) <= start || seg.Start >= m.End { //nolint:gosec // segment size is non-negative
			continue
		}
		if err := s.copyPageSegment(seg, start, m.End, &out); err != nil {
			return nil, err
		}
	}
	if len(out) != tailLen {
		return nil, fmt.Errorf("execution: incomplete capture tail")
	}
	return out, nil
}
func (s *streamStore) readPage(ctx context.Context, value string, size int) (Page, error) {
	gate, err := s.readerFence(ctx, unix.LOCK_SH)
	if err != nil {
		return Page{}, err
	}
	m, err := s.load()
	if err == nil && s.binding.Mode() != KindPTY {
		defer func() { _ = gate.Close() }()
		return s.pipePage(m, value, size)
	}
	if err == nil && s.confirmedManifest(m) {
		defer func() { _ = gate.Close() }()
		return s.pageData(m, value, size)
	}
	err = errors.Join(err, gate.Close())
	if err != nil {
		return Page{}, err
	}
	gate, err = s.readerFence(ctx, unix.LOCK_EX)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = gate.Close() }()
	m, err = s.load()
	if err != nil {
		return Page{}, err
	}
	if err := s.settle(ctx, m); err != nil {
		return Page{}, err
	}
	return s.pageData(m, value, size)
}
func (s *streamStore) pipePage(m manifest, value string, size int) (Page, error) {
	name := s.path("manifest.json")
	identity, err := s.confirmManifest(name)
	if err != nil {
		return Page{}, err
	}
	s.confirmed[name] = identity
	// Each selected segment is verified while the shared lock protects it.
	return s.pageData(m, value, size)
}

func (s *streamStore) pageData(m manifest, value string, size int) (Page, error) {
	p, start, err := m.page(s.binding, value)
	if err != nil {
		return p, err
	}
	end := pageEnd(start, m.End, size)
	for _, seg := range m.Segments {
		if seg.Start+uint64(seg.Size) <= start || seg.Start >= end {
			continue
		}
		if err := s.copyPageSegment(seg, start, end, &p.Bytes); err != nil {
			return Page{}, err
		}
	}
	if uint64(len(p.Bytes)) != end-start {
		return Page{}, fmt.Errorf("execution: incomplete capture page")
	}
	p.NextCursor = makeCursor(s.binding, s.stream, m.Generation, end)
	p.EOF = m.EOF && end == m.End
	return p, nil
}
func (o *Output) Result(ctx context.Context, b Binding) (Result, error) {
	var r Result
	if err := b.Validate(); err != nil {
		return r, err
	}
	if err := o.files.Read(attempt(b, "result.json"), &r); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if parentErr := o.files.check(filepath.Dir(attempt(b, "result.json")), false); parentErr != nil {
				return r, parentErr
			}
			return r, errResultPending
		}
		return r, err
	}
	if err := r.validate(b); err != nil {
		return Result{}, err
	}
	if err := o.confirmFinals(ctx, b, r.Streams); err != nil {
		return Result{}, err
	}
	return r, nil
}

// finalStream is one stream held under its exclusive fence while the result
// is confirmed.
type finalStream struct {
	store *streamStore
	gate  *Fence
	plan  settlePlan
}

func (s finalStream) release() {
	_ = s.gate.Close()
	s.store.leave()
}

// confirmFinals proves every stream's final capture and then the result
// durable, each stream held under its exclusive fence. Prerequisites are
// confirmed before what depends on them, because seeing a record does not
// prove its writer finished the barriers of what came before it: all
// streams' unconfirmed segments share one round, their manifests the next,
// and the result the last. Streams are held in result order, the same order
// every caller uses.
func (o *Output) confirmFinals(ctx context.Context, b Binding, streams []StreamResult) error {
	held := make([]finalStream, 0, len(streams))
	defer func() {
		for _, s := range held {
			s.release()
		}
	}()
	var segments, manifests []string
	for _, expected := range streams {
		s, err := o.holdFinal(ctx, b, expected)
		if err != nil {
			return err
		}
		held = append(held, s)
		segments = append(segments, s.plan.segments...)
		manifests = append(manifests, s.plan.manifests...)
	}
	if err := confirmInOrder(o.files, segments, manifests, []string{attempt(b, "result.json")}); err != nil {
		return err
	}
	for _, s := range held {
		s.store.confirmed = s.plan.keep
	}
	return nil
}

// holdFinal takes a stream's exclusive fence and checks its final manifest
// against the result. The caller releases what it returns.
func (o *Output) holdFinal(ctx context.Context, b Binding, expected StreamResult) (finalStream, error) {
	s, err := o.stream(b, expected.Stream)
	if err != nil {
		return finalStream{}, err
	}
	if err := s.enter(ctx); err != nil {
		return finalStream{}, err
	}
	gate, err := s.readerFence(ctx, unix.LOCK_EX)
	if err != nil {
		s.leave()
		return finalStream{}, err
	}
	held := finalStream{store: s, gate: gate}
	held.plan, err = s.planFinal(ctx, expected)
	if err != nil {
		held.release()
		return finalStream{}, err
	}
	return held, nil
}

func (s *streamStore) planFinal(ctx context.Context, expected StreamResult) (settlePlan, error) {
	m, err := s.load()
	if err != nil {
		return settlePlan{}, err
	}
	if m.Expired {
		if m.Generation != expected.Generation || offsetText(m.End) != expected.End || offsetText(m.Observed) != expected.ObservedEnd {
			return settlePlan{}, ErrResultExpired
		}
		return s.planSettle(ctx, m)
	}
	if !m.Closed || m.summary() != expected {
		return settlePlan{}, fmt.Errorf("execution: result capture mismatch")
	}
	return s.planSettle(ctx, m)
}
func (o *Output) binding(id, instance, host string) (Binding, error) {
	if !validToken(instance) {
		return Binding{}, ErrInstance
	}
	var b Binding
	if err := o.files.Read("execution/attempts/"+instance+"/reference.json", &b); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return b, ErrInstance
		}
		return b, err
	}
	if err := b.Validate(); err != nil {
		return Binding{}, err
	}
	if b.Host != host || b.Reservation.ID != id || b.Reservation.Instance != instance {
		return Binding{}, ErrInstance
	}
	return b, nil
}

// Binding resolves only an existing exact attempt. It does not create state or infer ownership.
func (o *Output) Binding(id, instance, host string) (Binding, error) {
	return o.binding(id, instance, host)
}

var errResultPending = errors.New("execution: result pending")

func IsResultPending(err error) bool { return errors.Is(err, errResultPending) }
func (s *streamStore) enter(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return executionBoundaryError("read capture", ctx.Err())
	}
}
func (s *streamStore) leave() { <-s.gate }

func ExpiredPage(b Binding, s StreamResult) (Page, error) {
	earliest, err := parseOffset(s.Earliest)
	if err != nil {
		return Page{}, err
	}
	end, err := parseOffset(s.End)
	if err != nil {
		return Page{}, err
	}
	observed, err := parseOffset(s.ObservedEnd)
	if err != nil || !validToken(s.Generation) {
		return Page{}, fmt.Errorf("execution: missing expired stream identity")
	}
	m := manifest{Stream: s.Stream, Generation: s.Generation, Earliest: earliest, End: end, Observed: observed, Closed: true, EOF: s.EOF, CloseReason: s.CloseReason, Loss: s.Loss, Expired: true}
	p, _, err := m.page(b, "")
	return p, err
}

func pageEnd(start, end uint64, size int) uint64 {
	if size < 1 || size > MaxPage {
		return start
	}
	return start + min(uint64(size), end-start)
}

func (s *streamStore) copyPageSegment(seg segment, start, end uint64, out *[]byte) error {
	return s.readSegment(seg, func(offset uint64, chunk []byte) {
		lo := max(start, offset)
		hi := min(end, offset+uint64(len(chunk)))
		if lo >= hi {
			return
		}
		*out = append(*out, chunk[lo-offset:hi-offset]...)
	})
}
