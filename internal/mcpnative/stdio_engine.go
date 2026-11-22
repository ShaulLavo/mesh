package mcpnative

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/transport"
)

const (
	// DefaultMaxStreams is the independent listen-stream limit per frontend (PLAN §2.5).
	DefaultMaxStreams = 4
)

// ErrStreamOverfull reports that a stream notification exceeded the frontend output buffer.
var ErrStreamOverfull = errors.New("MCP stream notification exceeded output buffer capacity")

// StdioEngineOptions configures the shared single-reader / single-writer
// stateless MCP 2026-07-28 stdio engine used by both `mesh mcp` and `mesh mcp connect`.
type StdioEngineOptions struct {
	Methods          mcpwire.Methods
	IsStreamMethod   func(method string) bool
	Handler          func(ctx context.Context, msg mcpwire.Message, responder *StdioResponder)
	CapacityFault    func(msg mcpwire.Message) *mcpwire.Fault
	LogPrefix        string
	Stderr           io.Writer
	MaxRequests      int
	MaxStreams       int
	MaxFrontendBytes int
	MaxConnBytes     int
	EOFDrainTimeout  time.Duration
	OnShutdown       func()
}

type queuedFrame struct {
	raw       []byte
	ownerKey  string
	releaseID *mcpwire.ID
}

type progressItem struct {
	key string
	raw []byte
}

type activeRequest struct {
	id            mcpwire.ID
	key           string
	stream        bool
	cancel        context.CancelFunc
	cancelled     bool
	completed     bool
	requestBytes  int
	reservedBytes int
}

// StdioResponder delivers correlated progress, notifications, terminal replies,
// and shared frontend byte-budget accounting for one active stdio request.
type StdioResponder struct {
	engine *StdioEngine
	entry  *activeRequest
}

// EnqueueProgress coalesces a progress notification for this request.
func (r *StdioResponder) EnqueueProgress(raw []byte) {
	r.engine.enqueueProgressRaw(r.entry, raw)
}

// EnqueueNotification queues a non-progress notification owned by this request.
// Unwritten notifications are dropped immediately if the request is cancelled.
func (r *StdioResponder) EnqueueNotification(raw []byte) error {
	return r.engine.enqueueNotification(r.entry, raw)
}

// Complete finishes this request with a validated JSON-RPC reply or fault.
func (r *StdioResponder) Complete(reply []byte, fault *mcpwire.Fault) {
	r.engine.completeActive(r.entry, reply, fault)
}

// ReserveBytes charges external buffer bytes (such as H3 request/inbound buffers)
// against the shared frontend byte budget.
func (r *StdioResponder) ReserveBytes(n int) bool {
	return r.engine.ReserveBytes(n)
}

// ReleaseBytes releases bytes previously reserved with ReserveBytes.
func (r *StdioResponder) ReleaseBytes(n int) {
	r.engine.ReleaseBytes(n)
}

// Budget returns the frontend's shared transport inbound byte budget.
func (r *StdioResponder) Budget() transport.InboundByteBudget {
	return r.engine
}

// StdioEngine runs the shared stateless MCP 2026-07-28 stdio loop.
type StdioEngine struct {
	methods        mcpwire.Methods
	isStreamMethod func(string) bool
	handler        func(context.Context, mcpwire.Message, *StdioResponder)
	capacityFault  func(mcpwire.Message) *mcpwire.Fault
	logPrefix      string
	stderr         io.Writer
	stderrCh       chan string
	maxReqs        int
	maxStreams     int
	maxBytes       int
	connBytes      int
	drainWait      time.Duration
	onShutdown     func()

	activeIDs mcpwire.ActiveIDs

	mu             sync.Mutex
	active         map[string]*activeRequest
	activeOrdinary int
	activeStreams  int
	inflightBytes  int
	h3Bytes        int
	queuedBytes    int
	writingBytes   int
	queuedFrames   []queuedFrame
	progressList   *list.List
	progressElem   map[string]*list.Element
	writerClosed   bool
	writerSignal   chan struct{}
	writeFailed    chan struct{}
	failOnce       sync.Once
	writeErr       error
	cancelRun      context.CancelFunc
}

// NewStdioEngine constructs the shared stateless MCP 2026-07-28 stdio engine.
func NewStdioEngine(opts StdioEngineOptions) (*StdioEngine, error) {
	if opts.Handler == nil {
		return nil, errors.New("mcp stdio engine: handler is required")
	}
	if len(opts.Methods) == 0 {
		return nil, errors.New("mcp stdio engine: at least one method is required")
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	prefix := opts.LogPrefix
	if prefix == "" {
		prefix = "mesh mcp"
	}
	maxReqs, maxStreams, maxBytes, connBytes, drainWait := resolveEngineLimits(opts)
	return &StdioEngine{
		methods:        opts.Methods,
		isStreamMethod: opts.IsStreamMethod,
		handler:        opts.Handler,
		capacityFault:  opts.CapacityFault,
		logPrefix:      prefix,
		stderr:         stderr,
		stderrCh:       make(chan string, stderrQueueSize),
		maxReqs:        maxReqs,
		maxStreams:     maxStreams,
		maxBytes:       maxBytes,
		connBytes:      connBytes,
		drainWait:      drainWait,
		onShutdown:     opts.OnShutdown,
		active:         make(map[string]*activeRequest),
		progressList:   list.New(),
		progressElem:   make(map[string]*list.Element),
		writerSignal:   make(chan struct{}, 1),
		writeFailed:    make(chan struct{}),
	}, nil
}

func resolveEngineLimits(opts StdioEngineOptions) (maxReqs, maxStreams, maxBytes, connBytes int, drainWait time.Duration) {
	maxReqs = opts.MaxRequests
	if maxReqs <= 0 {
		maxReqs = DefaultMaxRequests
	}
	maxStreams = opts.MaxStreams
	if maxStreams <= 0 && opts.IsStreamMethod != nil {
		maxStreams = DefaultMaxStreams
	}
	maxBytes = opts.MaxFrontendBytes
	if maxBytes <= 0 {
		maxBytes = DefaultFrontendBytes
	}
	connBytes = opts.MaxConnBytes
	if connBytes <= 0 {
		connBytes = DefaultConnBytes
	}
	drainWait = opts.EOFDrainTimeout
	if drainWait <= 0 {
		drainWait = defaultEOFDrainWait
	}
	return maxReqs, maxStreams, maxBytes, connBytes, drainWait
}

// ReserveBytes charges external buffer bytes (such as H3 request and inbound
// socket queues) against the frontend's shared 32 MiB byte budget.
func (s *StdioEngine) ReserveBytes(n int) bool {
	if n <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed || !s.fitsFrontendLocked(n) {
		return false
	}
	s.h3Bytes += n
	return true
}

// ReleaseBytes releases bytes previously reserved with ReserveBytes.
func (s *StdioEngine) ReleaseBytes(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.h3Bytes -= n
	if s.h3Bytes < 0 {
		s.h3Bytes = 0
	}
	s.mu.Unlock()
}

// Serve runs the single-reader / single-writer stdio loop until stdin EOF,
// context cancellation, framing loss, or stdout write failure.
func (s *StdioEngine) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	s.mu.Lock()
	s.cancelRun = cancelRun
	s.mu.Unlock()

	stopStderr := s.startStderrWriter()
	defer stopStderr()

	var workers sync.WaitGroup
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		s.writerLoop(out)
	}()

	readDone := make(chan error, 1)
	go func() {
		readDone <- s.readerLoop(runCtx, in, &workers)
	}()

	var readErr error
	select {
	case readErr = <-readDone:
	case <-s.writeFailed:
		abortStream(in)
	case <-ctx.Done():
		abortStream(in)
	}

	deadline := time.Now().Add(s.drainWait)
	cancelRun()
	s.cancelAllActive()

	shutdownDone := s.startShutdownHook()
	waitBounded(deadline, func() { workers.Wait() })
	s.closeWriter()
	if !waitChannelBounded(deadline, writerDone) {
		abortStream(out)
	}
	waitChannelBounded(deadline, shutdownDone)
	s.releaseRemainingIDs()

	if readErr != nil {
		return readErr
	}
	s.mu.Lock()
	writeErr := s.writeErr
	s.mu.Unlock()
	return writeErr
}

func (s *StdioEngine) startShutdownHook() <-chan struct{} {
	done := make(chan struct{})
	if s.onShutdown == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		s.onShutdown()
	}()
	return done
}

func (s *StdioEngine) startStderrWriter() func() {
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case line := <-s.stderrCh:
				_, _ = io.WriteString(s.stderr, line)
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

func (s *StdioEngine) logStderr(line string) {
	select {
	case s.stderrCh <- line:
	default:
	}
}

func (s *StdioEngine) readerLoop(ctx context.Context, in io.Reader, workers *sync.WaitGroup) error {
	reader := mcpwire.NewStdioReader(in)
	for {
		raw, err := reader.Read()
		if err != nil {
			keepGoing, readErr := s.handleReadError(err)
			if keepGoing {
				continue
			}
			return readErr
		}
		if ctx.Err() != nil {
			return nil //nolint:nilerr // Server shutdown cancels the reader cleanly.
		}
		s.dispatchLine(ctx, raw, workers)
	}
}

func (s *StdioEngine) handleReadError(err error) (bool, error) {
	if errors.Is(err, mcpwire.ErrTooLarge) {
		var id mcpwire.ID
		var oversized *mcpwire.OversizedStdioRequest
		if errors.As(err, &oversized) {
			id = oversized.ID
		}
		s.enqueueUnadmittedFault(&mcpwire.Fault{ID: id, Code: mcpwire.InvalidRequest, Message: mcpwire.ErrTooLarge.Error()})
		return true, nil
	}
	if errors.Is(err, mcpwire.ErrFramingLost) {
		s.logStderr(fmt.Sprintf("%s: %v\n", s.logPrefix, err))
		return false, fmt.Errorf("read MCP stdio input: %w", err)
	}
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	s.logStderr(fmt.Sprintf("%s: %v\n", s.logPrefix, err))
	return false, fmt.Errorf("read MCP stdio input: %w", err)
}

func (s *StdioEngine) dispatchLine(ctx context.Context, raw []byte, workers *sync.WaitGroup) {
	input := mcpwire.StdioInput(raw, s.methods)
	if input.Cancellation.Valid() {
		s.cancelRequest(input.Cancellation)
		return
	}
	if input.Ignored {
		if input.Message.Kind() == mcpwire.Notification && input.Message.Method() != "" {
			s.logStderr(fmt.Sprintf("%s: ignored notification %s\n", s.logPrefix, input.Message.Method()))
		}
		return
	}
	if input.Fault != nil {
		s.enqueueUnadmittedFault(input.Fault)
		return
	}
	if fault := s.activeIDs.Begin(input.Message); fault != nil {
		s.enqueueUnadmittedFault(fault)
		return
	}
	reqCtx, entry, admitted := s.admitRequest(ctx, input.Message, len(raw))
	if !admitted {
		s.activeIDs.End(input.Message.ID())
		s.enqueueUnadmittedFault(s.buildCapacityFault(input.Message))
		return
	}
	responder := &StdioResponder{engine: s, entry: entry}
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer entry.cancel()
		s.handler(reqCtx, input.Message, responder)
	}()
}

func (s *StdioEngine) buildCapacityFault(msg mcpwire.Message) *mcpwire.Fault {
	if s.capacityFault != nil {
		return s.capacityFault(msg)
	}
	return meshFault(msg.ID(), agentapi.ResourceLimit, true, "Frontend request capacity reached; retry after active requests finish.")
}

func (s *StdioEngine) totalBytesLocked() int {
	return s.inflightBytes + s.h3Bytes + s.queuedBytes + s.writingBytes
}

func (s *StdioEngine) stdioBytesLocked() int {
	return s.inflightBytes + s.queuedBytes + s.writingBytes
}

func (s *StdioEngine) fitsFrontendLocked(delta int) bool {
	if delta <= 0 {
		return true
	}
	return s.totalBytesLocked()+delta <= s.maxBytes
}

func (s *StdioEngine) fitsBudgetLocked(delta int) bool {
	if delta <= 0 {
		return true
	}
	return s.stdioBytesLocked()+delta <= s.connBytes && s.fitsFrontendLocked(delta)
}

func (s *StdioEngine) admitRequest(ctx context.Context, msg mcpwire.Message, rawBytes int) (context.Context, *activeRequest, bool) {
	id := msg.ID()
	key := id.Key()
	stream := s.isStreamMethod != nil && s.isStreamMethod(msg.Method())
	reserve := faultReservation(id)
	needed := rawBytes + reserve
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed || !s.fitsBudgetLocked(needed) {
		return nil, nil, false
	}
	if stream {
		if s.activeStreams >= s.maxStreams {
			return nil, nil, false
		}
		s.activeStreams++
	} else {
		if s.activeOrdinary >= s.maxReqs {
			return nil, nil, false
		}
		s.activeOrdinary++
	}
	reqCtx, cancel := context.WithCancel(ctx)
	entry := &activeRequest{
		id:            id,
		key:           key,
		stream:        stream,
		cancel:        cancel,
		requestBytes:  rawBytes,
		reservedBytes: reserve,
	}
	s.active[key] = entry
	s.inflightBytes += needed
	return reqCtx, entry, true
}

func (s *StdioEngine) completeActive(entry *activeRequest, reply []byte, fault *mcpwire.Fault) {
	frame, fallback, _ := prepareTerminalFrames(entry.id, reply, fault)

	s.mu.Lock()
	if entry.completed {
		s.mu.Unlock()
		return
	}
	entry.completed = true
	if current := s.active[entry.key]; current == entry {
		delete(s.active, entry.key)
		s.inflightBytes -= entry.requestBytes
		if entry.stream {
			s.activeStreams--
		} else {
			s.activeOrdinary--
		}
	}
	s.removeProgressLocked(entry.key)
	if entry.cancelled || s.writerClosed {
		s.dropOwnedFramesLocked(entry.key)
		s.inflightBytes -= entry.reservedBytes
		s.mu.Unlock()
		s.activeIDs.End(entry.id)
		return
	}
	chosen := frame
	if len(chosen) == 0 {
		chosen = fallback
	}
	if !s.fitsBudgetLocked(len(chosen) - entry.reservedBytes) {
		chosen = fallback
	}
	s.inflightBytes -= entry.reservedBytes
	if !s.fitsBudgetLocked(len(chosen)) {
		s.failChannelLocked(errOutputCapacity)
		s.mu.Unlock()
		s.activeIDs.End(entry.id)
		return
	}
	releaseID := entry.id
	s.queuedFrames = append(s.queuedFrames, queuedFrame{raw: chosen, ownerKey: entry.key, releaseID: &releaseID})
	s.queuedBytes += len(chosen)
	s.signalWriterLocked()
	s.mu.Unlock()
}

func (s *StdioEngine) enqueueUnadmittedFault(fault *mcpwire.Fault) {
	raw, err := fault.Reply()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed {
		return
	}
	if !s.fitsBudgetLocked(len(raw)) {
		s.failChannelLocked(errOutputCapacity)
		return
	}
	s.queuedFrames = append(s.queuedFrames, queuedFrame{raw: raw})
	s.queuedBytes += len(raw)
	s.signalWriterLocked()
}

func (s *StdioEngine) enqueueProgressRaw(entry *activeRequest, raw []byte) {
	if len(raw) == 0 {
		return
	}
	copied := append([]byte(nil), raw...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed || entry.cancelled || entry.completed {
		return
	}
	if elem, exists := s.progressElem[entry.key]; exists {
		item := elem.Value.(*progressItem)
		delta := len(copied) - len(item.raw)
		if delta > 0 && !s.fitsBudgetLocked(delta) {
			return
		}
		item.raw = copied
		s.queuedBytes += delta
		s.signalWriterLocked()
		return
	}
	if !s.fitsBudgetLocked(len(copied)) {
		return
	}
	elem := s.progressList.PushBack(&progressItem{key: entry.key, raw: copied})
	s.progressElem[entry.key] = elem
	s.queuedBytes += len(copied)
	s.signalWriterLocked()
}

func (s *StdioEngine) enqueueNotification(entry *activeRequest, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	copied := append([]byte(nil), raw...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed || entry.cancelled || entry.completed {
		return nil
	}
	if !s.fitsBudgetLocked(len(copied)) {
		if entry.stream {
			return ErrStreamOverfull
		}
		return errOutputCapacity
	}
	s.queuedFrames = append(s.queuedFrames, queuedFrame{raw: copied, ownerKey: entry.key})
	s.queuedBytes += len(copied)
	s.signalWriterLocked()
	return nil
}

func (s *StdioEngine) removeProgressLocked(key string) {
	elem, ok := s.progressElem[key]
	if !ok {
		return
	}
	item := elem.Value.(*progressItem)
	s.progressList.Remove(elem)
	delete(s.progressElem, key)
	s.queuedBytes -= len(item.raw)
}

func (s *StdioEngine) dropOwnedFramesLocked(key string) {
	if len(s.queuedFrames) == 0 {
		return
	}
	kept := s.queuedFrames[:0]
	for _, f := range s.queuedFrames {
		if f.ownerKey == key && f.releaseID == nil {
			s.queuedBytes -= len(f.raw)
			continue
		}
		kept = append(kept, f)
	}
	for i := len(kept); i < len(s.queuedFrames); i++ {
		s.queuedFrames[i] = queuedFrame{}
	}
	if len(kept) == 0 {
		s.queuedFrames = nil
		return
	}
	s.queuedFrames = kept
}

func (s *StdioEngine) signalWriterLocked() {
	select {
	case s.writerSignal <- struct{}{}:
	default:
	}
}

func (s *StdioEngine) failChannelLocked(err error) {
	if s.writeErr == nil {
		s.writeErr = err
	}
	s.writerClosed = true
	s.signalWriterLocked()
	s.failOnce.Do(func() { close(s.writeFailed) })
	if s.cancelRun != nil {
		s.cancelRun()
	}
}

func (s *StdioEngine) writerLoop(out io.Writer) {
	for {
		frame, ok := s.nextWriteFrame()
		if !ok {
			return
		}
		err := mcpwire.WriteStdio(out, frame.raw)
		s.finishWrite(frame)
		if err != nil {
			s.mu.Lock()
			s.failChannelLocked(fmt.Errorf("write MCP stdout: %w", err))
			s.mu.Unlock()
			return
		}
	}
}

func (s *StdioEngine) finishWrite(frame queuedFrame) {
	s.mu.Lock()
	s.writingBytes -= len(frame.raw)
	s.mu.Unlock()
	if frame.releaseID != nil {
		s.activeIDs.End(*frame.releaseID)
	}
}

func (s *StdioEngine) nextWriteFrame() (queuedFrame, bool) {
	for {
		s.mu.Lock()
		if frame, ok := s.popQueuedFrameLocked(); ok {
			s.mu.Unlock()
			return frame, true
		}
		if frame, ok := s.popProgressFrameLocked(); ok {
			s.mu.Unlock()
			return frame, true
		}
		if s.writerClosed {
			s.mu.Unlock()
			return queuedFrame{}, false
		}
		s.mu.Unlock()
		<-s.writerSignal
	}
}

func (s *StdioEngine) popQueuedFrameLocked() (queuedFrame, bool) {
	if len(s.queuedFrames) == 0 {
		return queuedFrame{}, false
	}
	frame := s.queuedFrames[0]
	s.queuedFrames[0] = queuedFrame{}
	s.queuedFrames = s.queuedFrames[1:]
	if len(s.queuedFrames) == 0 {
		s.queuedFrames = nil
	}
	s.queuedBytes -= len(frame.raw)
	s.writingBytes += len(frame.raw)
	return frame, true
}

func (s *StdioEngine) popProgressFrameLocked() (queuedFrame, bool) {
	front := s.progressList.Front()
	if front == nil {
		return queuedFrame{}, false
	}
	item := front.Value.(*progressItem)
	s.progressList.Remove(front)
	delete(s.progressElem, item.key)
	s.queuedBytes -= len(item.raw)
	s.writingBytes += len(item.raw)
	return queuedFrame{raw: item.raw}, true
}

func (s *StdioEngine) closeWriter() {
	s.mu.Lock()
	s.writerClosed = true
	s.signalWriterLocked()
	s.mu.Unlock()
}

func (s *StdioEngine) releaseRemainingIDs() {
	s.mu.Lock()
	var ids []mcpwire.ID
	for _, f := range s.queuedFrames {
		if f.releaseID != nil {
			ids = append(ids, *f.releaseID)
		}
	}
	s.queuedFrames = nil
	s.progressList.Init()
	clear(s.progressElem)
	s.queuedBytes = 0
	s.mu.Unlock()
	for _, id := range ids {
		s.activeIDs.End(id)
	}
}

func (s *StdioEngine) cancelRequest(id mcpwire.ID) {
	key := id.Key()
	s.mu.Lock()
	entry := s.active[key]
	if entry != nil && !entry.completed {
		entry.cancelled = true
		s.removeProgressLocked(key)
		s.dropOwnedFramesLocked(key)
	}
	s.mu.Unlock()
	if entry != nil {
		entry.cancel()
	}
}

func (s *StdioEngine) cancelAllActive() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for key, entry := range s.active {
		entry.cancelled = true
		s.removeProgressLocked(key)
		s.dropOwnedFramesLocked(key)
		cancels = append(cancels, entry.cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func abortStream(stream any) {
	type deadlineSetter interface {
		SetDeadline(time.Time) error
	}
	if ds, ok := stream.(deadlineSetter); ok {
		_ = ds.SetDeadline(time.Now())
	}
	if c, ok := stream.(io.Closer); ok {
		_ = c.Close()
	}
}

func waitBounded(deadline time.Time, fn func()) {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	waitChannelBounded(deadline, done)
}

func waitChannelBounded(deadline time.Time, done <-chan struct{}) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func faultReservation(id mcpwire.ID) int {
	raw, _ := id.MarshalJSON()
	return reservedErrorBytes + len(raw)
}
