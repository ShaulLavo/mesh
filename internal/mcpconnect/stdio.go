package mcpconnect

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
	DefaultMaxStreams    = 4
	DefaultFrontendBytes = 32 << 20
	DefaultConnBytes     = 8 << 20
	DefaultEOFDrainWait  = 2 * time.Second

	reservedErrorBytes = 4096
	meshRPCErrorCode   = -31000
	stderrQueueSize    = 16
	progressMethod     = "notifications/progress"
)

var (
	errOutputCapacity = errors.New("MCP output buffer capacity exceeded")
	errStreamOverfull = errors.New("MCP stream notification exceeded output buffer capacity")
)

// ServerOptions configures the direct H1 stdio server for mesh mcp connect.
type ServerOptions struct {
	Version          string
	HostID           string
	Service          string
	Forwarder        Forwarder
	Stderr           io.Writer
	MaxRequests      int
	MaxStreams       int
	MaxFrontendBytes int
	MaxConnBytes     int
	EOFDrainTimeout  time.Duration
	OnShutdown       func()
	// PassResults sends terminal results as the forwarder produced them. The
	// aggregate gateway sets it because it applies its own per-route server
	// identity; one service's identity must never fall back onto another's.
	PassResults bool
}

type queuedFrame struct {
	raw       []byte
	releaseID *mcpwire.ID
}

type activeRequest struct {
	id            mcpwire.ID
	key           string
	listen        bool
	cancel        context.CancelFunc
	cancelled     bool
	completed     bool
	requestBytes  int
	reservedBytes int
}

// Server runs the stateless MCP 2026-07-28 stdio frontend for one hosted service.
type Server struct {
	forwarder      Forwarder
	hostID         string
	service        string
	meshServerInfo json.RawMessage
	stderr         io.Writer
	stderrCh       chan string
	maxReqs        int
	maxStreams     int
	maxBytes       int
	connBytes      int
	drainWait      time.Duration
	onShutdown     func()
	passResults    bool

	activeIDs mcpwire.ActiveIDs

	mu               sync.Mutex
	active           map[string]*activeRequest
	activeOrdinary   int
	activeStreams    int
	inflightBytes    int
	queuedBytes      int
	writingBytes     int
	queuedFrames     []queuedFrame
	progressOrder    []string
	progressByKey    map[string][]byte
	lastUpstreamInfo json.RawMessage
	writerClosed     bool
	writerSignal     chan struct{}
	writeFailed      chan struct{}
	failOnce         sync.Once
	writeErr         error
	cancelRun        context.CancelFunc
}

// NewServer constructs a stateless direct-connect stdio server.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Forwarder == nil {
		return nil, errors.New("mcp connect: forwarder is required")
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	serverInfo, err := json.Marshal(map[string]string{"name": "mesh", "version": version})
	if err != nil {
		return nil, fmt.Errorf("encode MCP server info: %w", err)
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	maxReqs, maxStreams, maxBytes, connBytes, drainWait := resolveLimits(opts)
	return &Server{
		forwarder:      opts.Forwarder,
		hostID:         opts.HostID,
		service:        opts.Service,
		meshServerInfo: serverInfo,
		stderr:         stderr,
		stderrCh:       make(chan string, stderrQueueSize),
		maxReqs:        maxReqs,
		maxStreams:     maxStreams,
		maxBytes:       maxBytes,
		connBytes:      connBytes,
		drainWait:      drainWait,
		onShutdown:     opts.OnShutdown,
		passResults:    opts.PassResults,
		active:         make(map[string]*activeRequest),
		progressByKey:  make(map[string][]byte),
		writerSignal:   make(chan struct{}, 1),
		writeFailed:    make(chan struct{}),
	}, nil
}

func resolveLimits(opts ServerOptions) (maxReqs, maxStreams, maxBytes, connBytes int, drainWait time.Duration) {
	maxReqs = opts.MaxRequests
	if maxReqs <= 0 {
		maxReqs = DefaultMaxRequests
	}
	maxStreams = opts.MaxStreams
	if maxStreams <= 0 {
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
		drainWait = DefaultEOFDrainWait
	}
	return maxReqs, maxStreams, maxBytes, connBytes, drainWait
}

// Serve runs the single-reader / single-writer stdio loop until stdin EOF,
// context cancellation, framing loss, or stdout write failure.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
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

func (s *Server) startShutdownHook() <-chan struct{} {
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

func (s *Server) startStderrWriter() func() {
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

func (s *Server) logStderr(line string) {
	select {
	case s.stderrCh <- line:
	default:
	}
}

func (s *Server) readerLoop(ctx context.Context, in io.Reader, workers *sync.WaitGroup) error {
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
			return nil //nolint:nilerr // Shutdown cancels the reader cleanly.
		}
		s.dispatchLine(ctx, raw, workers)
	}
}

func (s *Server) handleReadError(err error) (bool, error) {
	if errors.Is(err, mcpwire.ErrTooLarge) {
		var id mcpwire.ID
		var oversized *mcpwire.OversizedStdioRequest
		if errors.As(err, &oversized) {
			id = oversized.ID
		}
		s.enqueueUnadmittedFault(&mcpwire.Fault{ID: id, Code: mcpwire.InvalidRequest, Message: mcpwire.ErrTooLarge.Error()})
		return true, nil
	}
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	s.logStderr(fmt.Sprintf("mesh mcp connect: %v\n", err))
	return false, fmt.Errorf("read MCP stdio input: %w", err)
}

func (s *Server) dispatchLine(ctx context.Context, raw []byte, workers *sync.WaitGroup) {
	input := mcpwire.StdioInput(raw, ForwardMethods)
	if input.Cancellation.Valid() {
		s.cancelRequest(input.Cancellation)
		return
	}
	if input.Ignored {
		if input.Message.Kind() == mcpwire.Notification && input.Message.Method() != "" {
			s.logStderr(fmt.Sprintf("mesh mcp connect: ignored notification %s\n", input.Message.Method()))
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
		s.enqueueUnadmittedFault(s.meshFault(input.Message.ID(), agentapi.ResourceLimit, true, "Frontend request capacity reached; retry after active requests finish."))
		return
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		s.executeRequest(reqCtx, input.Message, entry)
	}()
}

func (s *Server) admitRequest(ctx context.Context, msg mcpwire.Message, rawBytes int) (context.Context, *activeRequest, bool) {
	id := msg.ID()
	key := id.Key()
	listen := msg.Method() == mcpwire.MethodListen
	reserve := faultReservation(id)
	needed := rawBytes + reserve
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed || !s.fitsBudgetLocked(needed) {
		return nil, nil, false
	}
	if listen && s.activeStreams >= s.maxStreams {
		return nil, nil, false
	}
	if !listen && s.activeOrdinary >= s.maxReqs {
		return nil, nil, false
	}
	reqCtx, cancel := context.WithCancel(ctx)
	entry := &activeRequest{
		id:            id,
		key:           key,
		listen:        listen,
		cancel:        cancel,
		requestBytes:  rawBytes,
		reservedBytes: reserve,
	}
	s.active[key] = entry
	if listen {
		s.activeStreams++
	} else {
		s.activeOrdinary++
	}
	s.inflightBytes += needed
	return reqCtx, entry, true
}

func (s *Server) executeRequest(ctx context.Context, msg mcpwire.Message, entry *activeRequest) {
	defer entry.cancel()
	var terminalRaw []byte
	var terminalFault *mcpwire.Fault
	err := s.forwarder.Forward(ctx, msg, func(raw []byte, terminal bool) error {
		if !terminal {
			return s.enqueueNotification(entry, raw)
		}
		rewritten, fault := s.prepareResult(entry.id, raw)
		terminalRaw, terminalFault = rewritten, fault
		return nil
	})
	if err != nil && terminalRaw == nil && terminalFault == nil {
		if errors.Is(err, errStreamOverfull) {
			terminalFault = s.meshFault(entry.id, agentapi.ResourceLimit, true, "Stream notification exceeded frontend output buffer limit.")
		} else if ctx.Err() == nil {
			terminalFault = s.errorToFault(entry.id, err)
		}
	}
	s.completeActive(entry, terminalRaw, terminalFault)
}

func (s *Server) prepareResult(id mcpwire.ID, raw []byte) ([]byte, *mcpwire.Fault) {
	if s.passResults {
		return raw, nil
	}
	s.mu.Lock()
	fallback := s.lastUpstreamInfo
	s.mu.Unlock()
	rewritten, observed, err := rewriteResultServerInfo(raw, s.meshServerInfo, fallback)
	if len(observed) > 0 {
		s.mu.Lock()
		s.lastUpstreamInfo = observed
		s.mu.Unlock()
	}
	if errors.Is(err, mcpwire.ErrTooLarge) {
		return nil, s.meshFault(id, agentapi.MCPMessageTooLarge, false, "MCP reply exceeds the 3.5 MiB message limit.")
	}
	if err != nil {
		return nil, s.meshFault(id, agentapi.ServiceFailed, false, err.Error())
	}
	return rewritten, nil
}

func (s *Server) enqueueNotification(entry *activeRequest, raw []byte) error {
	if isProgressNotification(raw) {
		s.enqueueProgress(entry, raw)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed || entry.cancelled || entry.completed {
		return context.Canceled
	}
	if !s.fitsBudgetLocked(len(raw)) {
		return errStreamOverfull
	}
	s.queuedFrames = append(s.queuedFrames, queuedFrame{raw: cloneRaw(raw)})
	s.queuedBytes += len(raw)
	s.signalWriterLocked()
	return nil
}

func (s *Server) enqueueProgress(entry *activeRequest, raw []byte) {
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
	s.progressByKey[entry.key] = cloneRaw(raw)
	s.queuedBytes += delta
	s.signalWriterLocked()
}

func (s *Server) completeActive(entry *activeRequest, reply []byte, fault *mcpwire.Fault) {
	fallback := encodeBoundedFault(s.meshFault(entry.id, agentapi.ResourceLimit, true, "Frontend output buffer limit exceeded."), reservedErrorBytes)
	chosen := reply
	if fault != nil {
		chosen = encodeBoundedFault(fault, mcpwire.MaxMessage)
	} else if len(chosen) > mcpwire.MaxMessage {
		chosen = encodeBoundedFault(s.meshFault(entry.id, agentapi.MCPMessageTooLarge, false, "MCP reply exceeds the 3.5 MiB message limit."), mcpwire.MaxMessage)
	}

	s.mu.Lock()
	entry.completed = true
	if current := s.active[entry.key]; current == entry {
		delete(s.active, entry.key)
		if entry.listen {
			s.activeStreams--
		} else {
			s.activeOrdinary--
		}
		s.inflightBytes -= entry.requestBytes
	}
	if pending, ok := s.progressByKey[entry.key]; ok {
		delete(s.progressByKey, entry.key)
		s.queuedBytes -= len(pending)
	}
	if entry.cancelled || s.writerClosed || len(chosen) == 0 {
		s.inflightBytes -= entry.reservedBytes
		s.mu.Unlock()
		s.activeIDs.End(entry.id)
		return
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

func (s *Server) enqueueUnadmittedFault(fault *mcpwire.Fault) {
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

func (s *Server) errorToFault(id mcpwire.ID, err error) *mcpwire.Fault {
	var rf *RouteFailure
	if errors.As(err, &rf) {
		code := meshRPCErrorCode
		if rf.Code == agentapi.MCPStateInvalid {
			code = mcpwire.InvalidParams
		}
		return &mcpwire.Fault{
			ID:      id,
			Code:    code,
			Message: agentapi.CleanMessage(rf.Error()),
			Data:    map[string]any{"meshCode": string(rf.Code), "retryable": rf.Retryable, "host": rf.Host, "service": rf.Service},
		}
	}
	return s.meshFault(id, agentapi.StateUnavailable, true, err.Error())
}

func (s *Server) meshFault(id mcpwire.ID, code agentapi.Code, retryable bool, message string) *mcpwire.Fault {
	return &mcpwire.Fault{
		ID:      id,
		Code:    meshRPCErrorCode,
		Message: agentapi.CleanMessage(message),
		Data:    map[string]any{"meshCode": string(code), "retryable": retryable, "host": s.hostID, "service": s.service},
	}
}

func (s *Server) usedBytesLocked() int {
	return s.inflightBytes + s.queuedBytes + s.writingBytes
}

func (s *Server) fitsBudgetLocked(delta int) bool {
	if delta <= 0 {
		return true
	}
	used := s.usedBytesLocked() + delta
	return used <= s.connBytes && used <= s.maxBytes
}

func (s *Server) signalWriterLocked() {
	select {
	case s.writerSignal <- struct{}{}:
	default:
	}
}

func (s *Server) failChannelLocked(err error) {
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

func (s *Server) writerLoop(out io.Writer) {
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

func (s *Server) finishWrite(frame queuedFrame) {
	s.mu.Lock()
	s.writingBytes -= len(frame.raw)
	s.mu.Unlock()
	if frame.releaseID != nil {
		s.activeIDs.End(*frame.releaseID)
	}
}

func (s *Server) nextWriteFrame() (queuedFrame, bool) {
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

func (s *Server) popQueuedFrameLocked() (queuedFrame, bool) {
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

func (s *Server) popProgressFrameLocked() (queuedFrame, bool) {
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

func (s *Server) closeWriter() {
	s.mu.Lock()
	s.writerClosed = true
	s.signalWriterLocked()
	s.mu.Unlock()
}

func (s *Server) releaseRemainingIDs() {
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

func (s *Server) cancelRequest(id mcpwire.ID) {
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

func (s *Server) cancelAllActive() {
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

func isProgressNotification(raw []byte) bool {
	fields, err := decodeObject(raw)
	if err != nil {
		return false
	}
	var method string
	return json.Unmarshal(fields["method"], &method) == nil && method == progressMethod
}

func faultReservation(id mcpwire.ID) int {
	raw, _ := id.MarshalJSON()
	return reservedErrorBytes + len(raw)
}

func encodeBoundedFault(fault *mcpwire.Fault, maxBytes int) []byte {
	if fault == nil {
		return nil
	}
	limit := min(maxBytes, mcpwire.MaxMessage)
	if limit <= 0 {
		limit = mcpwire.MaxMessage
	}
	if raw, err := fault.Reply(); err == nil && len(raw) <= limit {
		return raw
	}
	nullID := &mcpwire.Fault{Code: fault.Code, Message: fault.Message, Data: fault.Data}
	if raw, err := nullID.Reply(); err == nil && len(raw) <= limit {
		return raw
	}
	minimal := &mcpwire.Fault{Code: fault.Code, Message: agentapi.CleanMessage(fault.Message)}
	raw, _ := minimal.Reply()
	return raw
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
