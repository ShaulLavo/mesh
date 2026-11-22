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
	discoverInstructions = "Call mesh_skill to read Mesh usage instructions, safety rules, and tool references before acting on a host."
)

var nativeMethods = mcpwire.Methods{
	mcpwire.MethodDiscover:  nil,
	mcpwire.MethodListTools: nil,
	mcpwire.MethodCallTool:  nil,
}

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

// StdioServer runs the stateless MCP 2026-07-28 stdio frontend.
type StdioServer struct {
	registry   *Registry
	controls   Controls
	serverInfo json.RawMessage
	stderr     io.Writer
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
	queuedFrames  [][]byte
	progressOrder []string
	progressByKey map[string][]byte
	writerClosed  bool
	writerSignal  chan struct{}
	writeFailed   chan struct{}
	writeErr      error
}

type activeRequest struct {
	id           mcpwire.ID
	cancel       context.CancelFunc
	cancelled    bool
	requestBytes int
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

	var workers sync.WaitGroup
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		s.writerLoop(out, cancelRun)
	}()

	readErr := s.readerLoop(runCtx, in, &workers)
	cancelRun()
	s.cancelAllActive()
	if s.onShutdown != nil {
		s.onShutdown()
	}
	s.waitWorkers(&workers)
	s.closeWriter()
	<-writerDone
	if readErr != nil {
		return readErr
	}
	s.mu.Lock()
	writeErr := s.writeErr
	s.mu.Unlock()
	return writeErr
}

func (s *StdioServer) readerLoop(ctx context.Context, in io.Reader, workers *sync.WaitGroup) error {
	reader := mcpwire.NewStdioReader(in)
	for {
		raw, err := reader.Read()
		if errors.Is(err, mcpwire.ErrTooLarge) {
			s.enqueueFault(&mcpwire.Fault{Code: mcpwire.InvalidRequest, Message: mcpwire.ErrTooLarge.Error()})
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			_, _ = fmt.Fprintf(s.stderr, "mesh mcp: %v\n", err)
			return fmt.Errorf("read MCP stdio input: %w", err)
		}
		if ctx.Err() != nil {
			return nil //nolint:nilerr // Server shutdown cancels the reader cleanly.
		}
		s.dispatchLine(ctx, raw, workers)
	}
}

func (s *StdioServer) dispatchLine(ctx context.Context, raw []byte, workers *sync.WaitGroup) {
	input := mcpwire.StdioInput(raw, nativeMethods)
	if input.Cancellation.Valid() {
		s.cancelRequest(input.Cancellation)
		return
	}
	if input.Ignored {
		if input.Message.Kind() == mcpwire.Notification && input.Message.Method() != "" {
			_, _ = fmt.Fprintf(s.stderr, "mesh mcp: ignored notification %s\n", input.Message.Method())
		}
		return
	}
	if input.Fault != nil {
		s.enqueueFault(input.Fault)
		return
	}
	if fault := s.activeIDs.Begin(input.Message); fault != nil {
		s.enqueueFault(fault)
		return
	}
	reqCtx, entry, admitted := s.admitRequest(ctx, input.Message, len(raw))
	if !admitted {
		s.activeIDs.End(input.Message.ID())
		s.enqueueMeshError(input.Message.ID(), agentapi.ResourceLimit, true, "Frontend request capacity reached; retry after active requests finish.")
		return
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		s.executeRequest(reqCtx, input.Message, entry)
	}()
}

func (s *StdioServer) admitRequest(ctx context.Context, msg mcpwire.Message, rawBytes int) (context.Context, *activeRequest, bool) {
	key := idKey(msg.ID())
	needed := rawBytes + reservedErrorBytes
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) >= s.maxReqs || s.inflightBytes+s.queuedBytes+needed > s.maxBytes {
		return nil, nil, false
	}
	reqCtx, cancel := context.WithCancel(ctx)
	entry := &activeRequest{id: msg.ID(), cancel: cancel, requestBytes: needed}
	s.active[key] = entry
	s.inflightBytes += needed
	return reqCtx, entry, true
}

func (s *StdioServer) executeRequest(ctx context.Context, msg mcpwire.Message, entry *activeRequest) {
	defer func() {
		entry.cancel()
		s.finishActive(entry)
	}()
	token := extractProgressToken(msg)
	notify := func(update ProgressUpdate) {
		if len(token) == 0 || s.isCancelled(entry) {
			return
		}
		s.enqueueProgress(idKey(msg.ID()), token, update)
	}
	reply, fault := s.handleMessage(ctx, msg, notify)
	if s.isCancelled(entry) {
		return
	}
	if fault != nil {
		s.enqueueFault(fault)
		return
	}
	s.enqueueFrame(reply, msg.ID())
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
	var failure *agentapi.Failure
	if errors.As(err, &failure) {
		outcome := CallOutcome{
			Envelope: errorEnvelope(operation, failure.Err.Target, failure.Err.Code, failure.Err.Message),
			IsError:  true,
		}
		raw, encodeErr := buildToolResult(outcome, operation)
		if encodeErr != nil {
			return nil, meshFault(id, agentapi.InternalError, false, encodeErr.Error())
		}
		return s.completeResult(msg, raw, 0)
	}
	return nil, meshFault(id, agentapi.InternalError, false, err.Error())
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

func (s *StdioServer) enqueueMeshError(id mcpwire.ID, code agentapi.Code, retryable bool, message string) {
	s.enqueueFault(meshFault(id, code, retryable, message))
}

func (s *StdioServer) enqueueFault(fault *mcpwire.Fault) {
	raw, err := fault.Reply()
	if err != nil {
		return
	}
	s.enqueueRaw(raw, true)
}

func (s *StdioServer) enqueueFrame(raw []byte, id mcpwire.ID) {
	if len(raw) > mcpwire.MaxMessage {
		s.enqueueMeshError(id, agentapi.MCPMessageTooLarge, false, "MCP reply exceeds the 3.5 MiB message limit.")
		return
	}
	if !s.enqueueRaw(raw, false) {
		s.enqueueMeshError(id, agentapi.ResourceLimit, true, "Frontend output buffer limit exceeded.")
	}
}

func (s *StdioServer) enqueueRaw(raw []byte, reserved bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writerClosed {
		return false
	}
	if !reserved && (s.queuedBytes+len(raw) > s.connBytes || s.inflightBytes+s.queuedBytes+len(raw) > s.maxBytes) {
		return false
	}
	s.queuedFrames = append(s.queuedFrames, raw)
	s.queuedBytes += len(raw)
	s.signalWriterLocked()
	return true
}

func (s *StdioServer) enqueueProgress(reqKey string, token json.RawMessage, update ProgressUpdate) {
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
	if s.writerClosed {
		return
	}
	prev := s.progressByKey[reqKey]
	delta := len(raw) - len(prev)
	if delta > 0 && (s.queuedBytes+delta > s.connBytes || s.inflightBytes+s.queuedBytes+delta > s.maxBytes) {
		return
	}
	if prev == nil {
		s.progressOrder = append(s.progressOrder, reqKey)
	}
	s.progressByKey[reqKey] = raw
	s.queuedBytes += delta
	s.signalWriterLocked()
}

func (s *StdioServer) signalWriterLocked() {
	select {
	case s.writerSignal <- struct{}{}:
	default:
	}
}

func (s *StdioServer) writerLoop(out io.Writer, cancelRun context.CancelFunc) {
	for {
		frame, ok := s.nextWriteFrame()
		if !ok {
			return
		}
		if err := mcpwire.WriteStdio(out, frame); err != nil {
			s.mu.Lock()
			s.writeErr = fmt.Errorf("write MCP stdout: %w", err)
			s.writerClosed = true
			s.mu.Unlock()
			close(s.writeFailed)
			cancelRun()
			return
		}
	}
}

func (s *StdioServer) nextWriteFrame() ([]byte, bool) {
	for {
		s.mu.Lock()
		if len(s.queuedFrames) > 0 {
			frame := s.queuedFrames[0]
			s.queuedFrames = s.queuedFrames[1:]
			s.queuedBytes -= len(frame)
			s.mu.Unlock()
			return frame, true
		}
		if len(s.progressOrder) > 0 {
			key := s.progressOrder[0]
			s.progressOrder = s.progressOrder[1:]
			frame := s.progressByKey[key]
			delete(s.progressByKey, key)
			if frame == nil {
				s.mu.Unlock()
				continue
			}
			s.queuedBytes -= len(frame)
			s.mu.Unlock()
			return frame, true
		}
		if s.writerClosed {
			s.mu.Unlock()
			return nil, false
		}
		s.mu.Unlock()
		<-s.writerSignal
	}
}

func (s *StdioServer) closeWriter() {
	s.mu.Lock()
	s.writerClosed = true
	s.signalWriterLocked()
	s.mu.Unlock()
}

func (s *StdioServer) cancelRequest(id mcpwire.ID) {
	key := idKey(id)
	s.mu.Lock()
	entry := s.active[key]
	if entry != nil {
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

func (s *StdioServer) isCancelled(entry *activeRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return entry.cancelled
}

func (s *StdioServer) finishActive(entry *activeRequest) {
	key := idKey(entry.id)
	s.mu.Lock()
	if current := s.active[key]; current == entry {
		delete(s.active, key)
		s.inflightBytes -= entry.requestBytes
	}
	if pending, ok := s.progressByKey[key]; ok {
		delete(s.progressByKey, key)
		s.queuedBytes -= len(pending)
	}
	s.mu.Unlock()
	s.activeIDs.End(entry.id)
}

func (s *StdioServer) waitWorkers(workers *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	timer := time.NewTimer(s.drainWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func idKey(id mcpwire.ID) string {
	raw, _ := id.MarshalJSON()
	return string(raw)
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
