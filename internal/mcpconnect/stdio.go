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
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/mcpwire"
)

const (
	DefaultMaxRequests   = mcpnative.DefaultMaxRequests
	DefaultMaxStreams    = mcpnative.DefaultMaxStreams
	DefaultFrontendBytes = mcpnative.DefaultFrontendBytes
	DefaultConnBytes     = mcpnative.DefaultConnBytes
	DefaultEOFDrainWait  = 2 * time.Second

	meshRPCErrorCode = -31000
	progressMethod   = "notifications/progress"
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
}

// Server runs the stateless MCP 2026-07-28 stdio frontend for one hosted service
// on top of the shared mcpnative.StdioEngine.
type Server struct {
	forwarder      Forwarder
	hostID         string
	service        string
	meshServerInfo json.RawMessage
	engine         *mcpnative.StdioEngine

	upstreamMu       sync.Mutex
	lastUpstreamInfo json.RawMessage
}

// NewServer constructs a stateless direct-connect stdio server backed by the
// shared mcpnative.StdioEngine.
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
	server := &Server{
		forwarder:      opts.Forwarder,
		hostID:         opts.HostID,
		service:        opts.Service,
		meshServerInfo: serverInfo,
	}
	engine, err := mcpnative.NewStdioEngine(mcpnative.StdioEngineOptions{
		Methods:          ForwardMethods,
		IsStreamMethod:   func(method string) bool { return method == mcpwire.MethodListen },
		Handler:          server.handleRequest,
		CapacityFault:    server.capacityFault,
		LogPrefix:        "mesh mcp connect",
		Stderr:           opts.Stderr,
		MaxRequests:      opts.MaxRequests,
		MaxStreams:       opts.MaxStreams,
		MaxFrontendBytes: opts.MaxFrontendBytes,
		MaxConnBytes:     opts.MaxConnBytes,
		EOFDrainTimeout:  opts.EOFDrainTimeout,
		OnShutdown:       opts.OnShutdown,
	})
	if err != nil {
		return nil, fmt.Errorf("create MCP connect stdio engine: %w", err)
	}
	server.engine = engine
	return server, nil
}

// Serve runs the shared single-reader / single-writer stdio loop until stdin EOF,
// context cancellation, framing loss, or stdout write failure.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if err := s.engine.Serve(ctx, in, out); err != nil {
		return fmt.Errorf("serve MCP connect stdio: %w", err)
	}
	return nil
}

func (s *Server) capacityFault(msg mcpwire.Message) *mcpwire.Fault {
	return s.meshFault(msg.ID(), agentapi.ResourceLimit, true, "Frontend request capacity reached; retry after active requests finish.")
}

func (s *Server) handleRequest(ctx context.Context, msg mcpwire.Message, responder *mcpnative.StdioResponder) {
	var terminalRaw []byte
	var terminalFault *mcpwire.Fault
	err := s.forwarder.Forward(ctx, msg, responder.Budget(), func(raw []byte, terminal bool) error {
		if !terminal {
			return s.handleNotification(raw, responder)
		}
		reply, fault := s.prepareTerminal(msg, raw)
		terminalRaw = reply
		terminalFault = fault
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			responder.Complete(nil, nil)
			return
		}
		reply, fault := s.handleForwardError(msg, err)
		responder.Complete(reply, fault)
		return
	}
	responder.Complete(terminalRaw, terminalFault)
}

func (s *Server) handleNotification(raw []byte, responder *mcpnative.StdioResponder) error {
	if isProgressNotification(raw) {
		responder.EnqueueProgress(raw)
		return nil
	}
	if err := responder.EnqueueNotification(raw); err != nil {
		return fmt.Errorf("enqueue MCP notification: %w", err)
	}
	return nil
}

func isProgressNotification(raw []byte) bool {
	var header struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(raw, &header) == nil && header.Method == progressMethod
}

func (s *Server) prepareTerminal(req mcpwire.Message, raw []byte) ([]byte, *mcpwire.Fault) {
	fallback := s.upstreamInfo()
	if converted, matched, err := translateHostMeshToolError(req, raw, s.hostID, s.service, s.meshServerInfo, fallback); matched || err != nil {
		if err != nil {
			return nil, s.handleRewriteError(req, err)
		}
		return converted, nil
	}
	rewritten, observed, err := rewriteForwardedResult(req.Method(), raw, s.meshServerInfo, fallback)
	if len(observed) > 0 {
		s.rememberUpstreamInfo(observed)
	}
	if err != nil {
		return nil, s.handleRewriteError(req, err)
	}
	return rewritten, nil
}

func (s *Server) handleRewriteError(req mcpwire.Message, err error) *mcpwire.Fault {
	if errors.Is(err, mcpwire.ErrTooLarge) {
		return s.meshFault(req.ID(), agentapi.MCPMessageTooLarge, false, "MCP reply exceeds the 3.5 MiB message limit after metadata preservation.")
	}
	return s.meshFault(req.ID(), agentapi.ServiceFailed, false, err.Error())
}

func (s *Server) handleForwardError(req mcpwire.Message, err error) ([]byte, *mcpwire.Fault) {
	code, retryable, host, service, message := s.classifyForwardError(err)
	if req.Method() == mcpwire.MethodCallTool {
		reply, encodeErr := buildToolErrorResult(req.ID(), code, retryable, host, service, message, s.meshServerInfo, s.upstreamInfo())
		if encodeErr == nil {
			return reply, nil
		}
	}
	return nil, s.meshFaultFor(req.ID(), code, retryable, host, service, message)
}

func (s *Server) classifyForwardError(err error) (code agentapi.Code, retryable bool, host, service, message string) {
	host, service = s.hostID, s.service
	var rf *RouteFailure
	if errors.As(err, &rf) {
		if rf.Host != "" {
			host = rf.Host
		}
		if rf.Service != "" {
			service = rf.Service
		}
		return rf.Code, rf.Retryable, host, service, rf.Error()
	}
	if errors.Is(err, mcpnative.ErrStreamOverfull) {
		return agentapi.ResourceLimit, true, host, service, "Frontend stream buffer limit reached; reconnect to resume."
	}
	return agentapi.ServiceFailed, false, host, service, err.Error()
}

func (s *Server) meshFault(id mcpwire.ID, code agentapi.Code, retryable bool, message string) *mcpwire.Fault {
	return s.meshFaultFor(id, code, retryable, s.hostID, s.service, message)
}

func (s *Server) meshFaultFor(id mcpwire.ID, code agentapi.Code, retryable bool, host, service, message string) *mcpwire.Fault {
	data := map[string]any{
		"meshCode":  string(code),
		"retryable": retryable,
	}
	if host != "" {
		data["host"] = host
	}
	if service != "" {
		data["service"] = service
	}
	return &mcpwire.Fault{
		ID:      id,
		Code:    meshRPCErrorCode,
		Message: agentapi.CleanMessage(message),
		Data:    data,
	}
}

func (s *Server) upstreamInfo() json.RawMessage {
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	return cloneRaw(s.lastUpstreamInfo)
}

func (s *Server) rememberUpstreamInfo(info json.RawMessage) {
	s.upstreamMu.Lock()
	s.lastUpstreamInfo = cloneRaw(info)
	s.upstreamMu.Unlock()
}
