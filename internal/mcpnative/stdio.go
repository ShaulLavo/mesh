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
	registry   *Registry
	controls   Controls
	serverInfo json.RawMessage
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
	registry := opts.Registry
	if registry == nil {
		registry = DefaultRegistry()
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
	maxReqs := opts.MaxRequests
	if maxReqs <= 0 {
		maxReqs = DefaultMaxRequests
	}
	maxBytes := opts.MaxFrontendBytes
	if maxBytes <= 0 {
		maxBytes = DefaultFrontendBytes
	}
	connBytes := opts.MaxConnBytes
	if connBytes <= 0 {
		connBytes = DefaultConnBytes
	}
	drainWait := opts.EOFDrainTimeout
	if drainWait <= 0 {
		drainWait = defaultEOFDrainWait
	}
	return &StdioServer{
		registry:      registry,
		controls:      opts.Controls,
		serverInfo:    serverInfo,
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

func waitBounded(deadline time.Time, fn func()) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	return waitChannelBounded(deadline, done)
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
	notify := func(update ProgressUpdate) {
		if len(token) == 0 {
			return
		}
		s.enqueueProgress(entry, token, update)
	}
	reply, fault := s.handleMessage(ctx, msg, notify)
	s.completeActive(entry, reply, fault)
}

func (s *StdioServer) completeActive(entry *activeRequest, reply []byte, fault *mcpwire.Fault) {
	frame, fallback := s.prepareTerminalFrames(entry.id, reply, fault)

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

func (s *StdioServer) prepareTerminalFrames(id mcpwire.ID, reply []byte, fault *mcpwire.Fault) (primary, fallback []byte) {
	fallback, _ = meshFault(id, agentapi.ResourceLimit, true, "Frontend output buffer limit exceeded.").Reply()
	if fault != nil {
		primary, _ = fault.Reply()
		return primary, fallback
	}
	if len(reply) > mcpwire.MaxMessage {
		primary, _ = meshFault(id, agentapi.MCPMessageTooLarge, false, "MCP reply exceeds the 3.5 MiB message limit.").Reply()
		return primary, fallback
	}
	return reply, fallback
}

func (s *StdioServer) handleMessage(ctx context.Context, msg mcpwire.Message, notify func(ProgressUpdate)) ([]byte, *mcpwire.Fault) {
	switch msg.Method() {
	case mcpwire.MethodDiscover:
		return s.completeResult(msg, discoverPayload(), defaultDiscoverTTLMS)
	case mcpwire.MethodListTools:
		return s.handleListTools(msg)
	case mcpwire.MethodCallTool:
		return s.handleCallTool(ctx, msg, notify)
	default:
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: mcpwire.MethodNotFound, Message: "MCP method not found"}
	}
}

func discoverPayload() json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"capabilities": map[string]any{
			"tools": map[string]bool{"listChanged": false},
		},
		"instructions": discoverInstructions,
	})
	return raw
}

func (s *StdioServer) handleListTools(msg mcpwire.Message) ([]byte, *mcpwire.Fault) {
	var params struct {
		Cursor string `json:"cursor"`
	}
	if len(msg.Params()) > 0 {
		_ = json.Unmarshal(msg.Params(), &params)
	}
	if params.Cursor != "" {
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: mcpwire.InvalidParams, Message: "Invalid pagination cursor"}
	}
	raw, err := json.Marshal(map[string]any{"tools": s.registry.Definitions()})
	if err != nil {
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: meshRPCErrorCode, Message: err.Error()}
	}
	return s.completeResult(msg, raw, defaultDiscoverTTLMS)
}

func (s *StdioServer) handleCallTool(ctx context.Context, msg mcpwire.Message, notify func(ProgressUpdate)) ([]byte, *mcpwire.Fault) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params(), &params); err != nil {
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: mcpwire.InvalidParams, Message: "Invalid tools/call parameters"}
	}
	spec, ok := s.registry.Lookup(params.Name)
	if !ok {
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: mcpwire.InvalidParams, Message: fmt.Sprintf("Unknown tool %q", params.Name)}
	}
	outcome, err := spec.Handler(ctx, s.controls, params.Arguments, notify)
	if err != nil {
		return s.toolHandlerError(msg, spec.Operation, err)
	}
	resultRaw, err := buildToolResult(outcome, spec.Operation)
	if err != nil {
		return nil, meshFault(msg.ID(), agentapi.InternalError, false, err.Error())
	}
	return s.completeResult(msg, resultRaw, 0)
}

func (s *StdioServer) toolHandlerError(msg mcpwire.Message, operation string, err error) ([]byte, *mcpwire.Fault) {
	id := msg.ID()
	var badParam *paramError
	if errors.As(err, &badParam) || errors.Is(err, ErrInvalidHandle) {
		return nil, &mcpwire.Fault{ID: id, Code: mcpwire.InvalidParams, Message: err.Error()}
	}
	var envelope agentapi.Envelope
	var failure *agentapi.Failure
	if errors.As(err, &failure) {
		item := failure.Err
		item.Operation = operation
		item.Message = agentapi.CleanMessage(item.Message)
		envelope = errorEnvelopeFromItem(operation, item)
	} else {
		envelope = errorEnvelope(operation, "", agentapi.StateUnavailable, agentapi.CleanMessage(err.Error()))
	}
	raw, encodeErr := buildToolResult(CallOutcome{Envelope: envelope, IsError: true}, operation)
	if encodeErr != nil {
		return nil, meshFault(id, agentapi.InternalError, false, encodeErr.Error())
	}
	return s.completeResult(msg, raw, 0)
}

func errorEnvelopeFromItem(operation string, item agentapi.Error) agentapi.Envelope {
	id, _ := agentapi.NewRequestID()
	return agentapi.Envelope{
		SchemaVersion: agentapi.SchemaVersion,
		Operation:     operation,
		Status:        agentapi.StatusError,
		RequestID:     id,
		Errors:        []agentapi.Error{item},
	}
}

func (s *StdioServer) completeResult(req mcpwire.Message, result json.RawMessage, ttlMS int64) ([]byte, *mcpwire.Fault) {
	reply, err := mcpwire.Complete(req, result, s.serverInfo, ttlMS)
	if errors.Is(err, mcpwire.ErrTooLarge) {
		return nil, meshFault(req.ID(), agentapi.MCPMessageTooLarge, false, "MCP reply exceeds the 3.5 MiB message limit.")
	}
	if err != nil {
		return nil, meshFault(req.ID(), agentapi.InternalError, false, err.Error())
	}
	return reply, nil
}

func meshFault(id mcpwire.ID, code agentapi.Code, retryable bool, message string) *mcpwire.Fault {
	return &mcpwire.Fault{
		ID:      id,
		Code:    meshRPCErrorCode,
		Message: agentapi.CleanMessage(message),
		Data:    map[string]any{"meshCode": string(code), "retryable": retryable},
	}
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
	params := map[string]any{"progressToken": token, "progress": update.Progress}
	if update.Total != nil {
		params["total"] = *update.Total
	}
	if update.Message != "" {
		params["message"] = agentapi.CleanMessage(update.Message)
	}
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params":  params,
	})
	if err != nil || len(raw) > mcpwire.MaxMessage {
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

func extractProgressToken(msg mcpwire.Message) json.RawMessage {
	if len(msg.Params()) == 0 {
		return nil
	}
	var params struct {
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(msg.Params(), &params) != nil {
		return nil
	}
	if len(params.Meta.ProgressToken) == 0 || string(params.Meta.ProgressToken) == "null" {
		return nil
	}
	return params.Meta.ProgressToken
}
