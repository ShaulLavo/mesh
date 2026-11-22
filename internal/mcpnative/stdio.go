package mcpnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

const (
	DefaultMaxRequests   = 64
	DefaultFrontendBytes = 32 << 20
	DefaultConnBytes     = 8 << 20
	defaultDiscoverTTLMS = 300000
	defaultEOFDrainWait  = 2 * time.Second
	reservedErrorBytes   = 4096
	meshRPCErrorCode     = -31000
	stderrQueueSize      = 16
	discoverInstructions = "Call mesh_skill to read Mesh usage instructions, safety rules, and tool references before acting on a host."
)

var (
	errOutputCapacity = errors.New("MCP output buffer capacity exceeded")
	nativeMethods     = mcpwire.Methods{
		mcpwire.MethodDiscover:  nil,
		mcpwire.MethodListTools: nil,
		mcpwire.MethodCallTool:  nil,
	}
)

// ServerOptions configures the native stdio MCP frontend.
type ServerOptions struct {
	Version          string
	Registry         *Registry
	Controls         Controls
	Stderr           io.Writer
	MaxRequests      int
	MaxFrontendBytes int
	MaxConnBytes     int
	EOFDrainTimeout  time.Duration
	OnShutdown       func()
}

type queuedFrame struct {
	raw       []byte
	releaseID *mcpwire.ID
}

// StdioServer runs the stateless MCP 2026-07-28 stdio frontend.
type StdioServer struct {
	core       *dispatcher
	stderr     io.Writer
	stderrCh   chan string
	maxReqs    int
	maxBytes   int
	connBytes  int
	drainWait  time.Duration
	onShutdown func()

	activeIDs mcpwire.ActiveIDs

	mu            sync.Mutex
	active        map[string]*activeRequest
	inflightBytes int
	queuedBytes   int
	writingBytes  int
	queuedFrames  []queuedFrame
	progressOrder []string
	progressByKey map[string][]byte
	writerClosed  bool
	writerSignal  chan struct{}
	writeFailed   chan struct{}
	failOnce      sync.Once
	writeErr      error
	cancelRun     context.CancelFunc
}

type activeRequest struct {
	id            mcpwire.ID
	key           string
	cancel        context.CancelFunc
	cancelled     bool
	completed     bool
	requestBytes  int
	reservedBytes int
}

// NewStdioServer constructs a stateless native MCP stdio server.
func NewStdioServer(opts ServerOptions) (*StdioServer, error) {
	core, err := newDispatcher(opts)
	if err != nil {
		return nil, err
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	maxReqs, maxBytes, connBytes, drainWait := serverLimits(opts)
	return &StdioServer{
		core:          core,
		stderr:        stderr,
		stderrCh:      make(chan string, stderrQueueSize),
		maxReqs:       maxReqs,
		maxBytes:      maxBytes,
		connBytes:     connBytes,
		drainWait:     drainWait,
		onShutdown:    opts.OnShutdown,
		active:        make(map[string]*activeRequest),
		progressByKey: make(map[string][]byte),
		writerSignal:  make(chan struct{}, 1),
		writeFailed:   make(chan struct{}),
	}, nil
}

// Serve runs the single-reader / single-writer stdio loop until stdin EOF,
// context cancellation, framing loss, or stdout write failure.
func (s *StdioServer) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
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

func (s *StdioServer) startShutdownHook() <-chan struct{} {
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

func (s *StdioServer) startStderrWriter() func() {
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
	return func() {
		close(stop)
	}
}

func (s *StdioServer) logStderr(line string) {
	select {
	case s.stderrCh <- line:
	default:
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

func (s *StdioServer) readerLoop(ctx context.Context, in io.Reader, workers *sync.WaitGroup) error {
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

func (s *StdioServer) handleReadError(err error) (bool, error) {
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
		s.logStderr(fmt.Sprintf("mesh mcp: %v\n", err))
		return false, fmt.Errorf("read MCP stdio input: %w", err)
	}
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	s.logStderr(fmt.Sprintf("mesh mcp: %v\n", err))
	return false, fmt.Errorf("read MCP stdio input: %w", err)
}

func (s *StdioServer) dispatchLine(ctx context.Context, raw []byte, workers *sync.WaitGroup) {
	input := mcpwire.StdioInput(raw, nativeMethods)
	if input.Cancellation.Valid() {
		s.cancelRequest(input.Cancellation)
		return
	}
	if input.Ignored {
		if input.Message.Kind() == mcpwire.Notification && input.Message.Method() != "" {
			s.logStderr(fmt.Sprintf("mesh mcp: ignored notification %s\n", input.Message.Method()))
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
		s.enqueueUnadmittedFault(meshFault(input.Message.ID(), agentapi.ResourceLimit, true, "Frontend request capacity reached; retry after active requests finish."))
		return
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		s.executeRequest(reqCtx, input.Message, entry)
	}()
}

func faultReservation(id mcpwire.ID) int {
	raw, _ := id.MarshalJSON()
	return reservedErrorBytes + len(raw)
}

func (s *StdioServer) usedBytesLocked() int {
	return s.inflightBytes + s.queuedBytes + s.writingBytes
}

func (s *StdioServer) fitsBudgetLocked(delta int) bool {
	if delta <= 0 {
		return true
	}
	used := s.usedBytesLocked() + delta
	return used <= s.connBytes && used <= s.maxBytes
}

func (s *StdioServer) admitRequest(ctx context.Context, msg mcpwire.Message, rawBytes int) (context.Context, *activeRequest, bool) {
	id := msg.ID()
	key := id.Key()
	reserve := faultReservation(id)
	needed := rawBytes + reserve
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed || len(s.active) >= s.maxReqs || !s.fitsBudgetLocked(needed) {
		return nil, nil, false
	}
	reqCtx, cancel := context.WithCancel(ctx)
	entry := &activeRequest{
		id:            id,
		key:           key,
		cancel:        cancel,
		requestBytes:  rawBytes,
		reservedBytes: reserve,
	}
	s.active[key] = entry
	s.inflightBytes += needed
	return reqCtx, entry, true
}

func (s *StdioServer) executeRequest(ctx context.Context, msg mcpwire.Message, entry *activeRequest) {
	defer entry.cancel()
	token := extractProgressToken(msg)
	var notify func(ProgressUpdate)
	if len(token) > 0 {
		notify = func(update ProgressUpdate) {
			s.enqueueProgress(entry, token, update)
		}
	}
	reply, fault := s.core.handleMessage(ctx, msg, notify)
	s.completeActive(entry, reply, fault)
}

func (s *StdioServer) completeActive(entry *activeRequest, reply []byte, fault *mcpwire.Fault) {
	frame, fallback, _ := prepareTerminalFrames(entry.id, reply, fault)

	s.mu.Lock()
	entry.completed = true
	if current := s.active[entry.key]; current == entry {
		delete(s.active, entry.key)
		s.inflightBytes -= entry.requestBytes
	}
	if pending, ok := s.progressByKey[entry.key]; ok {
		delete(s.progressByKey, entry.key)
		s.queuedBytes -= len(pending)
	}
	if entry.cancelled || s.writerClosed {
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
	s.queuedFrames = append(s.queuedFrames, queuedFrame{raw: chosen, releaseID: &releaseID})
	s.queuedBytes += len(chosen)
	s.signalWriterLocked()
	s.mu.Unlock()
}

func (s *StdioServer) enqueueUnadmittedFault(fault *mcpwire.Fault) {
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

func (s *StdioServer) enqueueProgress(entry *activeRequest, token json.RawMessage, update ProgressUpdate) {
	raw, ok := marshalProgressNotification(token, update)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed || entry.cancelled || entry.completed {
		return
	}
	prev := s.progressByKey[entry.key]
	delta := len(raw) - len(prev)
	if delta > 0 && !s.fitsBudgetLocked(delta) {
		return
	}
	if prev == nil {
		s.progressOrder = append(s.progressOrder, entry.key)
	}
	s.progressByKey[entry.key] = raw
	s.queuedBytes += delta
	s.signalWriterLocked()
}

func (s *StdioServer) signalWriterLocked() {
	select {
	case s.writerSignal <- struct{}{}:
	default:
	}
}

func (s *StdioServer) failChannelLocked(err error) {
	if s.writeErr == nil {
		s.writeErr = err
	}
	s.writerClosed = true
	s.signalWriterLocked()
	s.failOnce.Do(func() {
		close(s.writeFailed)
	})
	if s.cancelRun != nil {
		s.cancelRun()
	}
}

func (s *StdioServer) writerLoop(out io.Writer) {
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

func (s *StdioServer) finishWrite(frame queuedFrame) {
	s.mu.Lock()
	s.writingBytes -= len(frame.raw)
	s.mu.Unlock()
	if frame.releaseID != nil {
		s.activeIDs.End(*frame.releaseID)
	}
}

func (s *StdioServer) nextWriteFrame() (queuedFrame, bool) {
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

func (s *StdioServer) popQueuedFrameLocked() (queuedFrame, bool) {
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

func (s *StdioServer) popProgressFrameLocked() (queuedFrame, bool) {
	for len(s.progressOrder) > 0 {
		key := s.progressOrder[0]
		s.progressOrder[0] = ""
		s.progressOrder = s.progressOrder[1:]
		if len(s.progressOrder) == 0 {
			s.progressOrder = nil
		}
		raw := s.progressByKey[key]
		delete(s.progressByKey, key)
		if raw == nil {
			continue
		}
		s.queuedBytes -= len(raw)
		s.writingBytes += len(raw)
		return queuedFrame{raw: raw}, true
	}
	return queuedFrame{}, false
}

func (s *StdioServer) closeWriter() {
	s.mu.Lock()
	s.writerClosed = true
	s.signalWriterLocked()
	s.mu.Unlock()
}

func (s *StdioServer) releaseRemainingIDs() {
	s.mu.Lock()
	var ids []mcpwire.ID
	for _, f := range s.queuedFrames {
		if f.releaseID != nil {
			ids = append(ids, *f.releaseID)
		}
	}
	s.queuedFrames = nil
	s.queuedBytes = 0
	s.mu.Unlock()
	for _, id := range ids {
		s.activeIDs.End(id)
	}
}

func (s *StdioServer) cancelRequest(id mcpwire.ID) {
	key := id.Key()
	s.mu.Lock()
	entry := s.active[key]
	if entry != nil && !entry.completed {
		entry.cancelled = true
	}
	s.mu.Unlock()
	if entry != nil {
		entry.cancel()
	}
}

func (s *StdioServer) cancelAllActive() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, entry := range s.active {
		entry.cancelled = true
		cancels = append(cancels, entry.cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
