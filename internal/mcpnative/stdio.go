package mcpnative

import (
	"context"
	"errors"
	"io"
	"time"

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

// StdioServer runs the stateless MCP 2026-07-28 stdio frontend for native tools.
type StdioServer struct {
	core   *dispatcher
	engine *StdioEngine
}

// NewStdioServer constructs a stateless native MCP stdio server backed by the
// shared StdioEngine.
func NewStdioServer(opts ServerOptions) (*StdioServer, error) {
	core, err := newDispatcher(opts)
	if err != nil {
		return nil, err
	}
	maxReqs, maxBytes, connBytes, drainWait := serverLimits(opts)
	server := &StdioServer{core: core}
	engine, err := NewStdioEngine(StdioEngineOptions{
		Methods:          nativeMethods,
		Handler:          server.handleRequest,
		LogPrefix:        "mesh mcp",
		Stderr:           opts.Stderr,
		MaxRequests:      maxReqs,
		MaxFrontendBytes: maxBytes,
		MaxConnBytes:     connBytes,
		EOFDrainTimeout:  drainWait,
		OnShutdown:       opts.OnShutdown,
	})
	if err != nil {
		return nil, err
	}
	server.engine = engine
	return server, nil
}

// Serve runs the single-reader / single-writer stdio loop until stdin EOF,
// context cancellation, framing loss, or stdout write failure.
func (s *StdioServer) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return s.engine.Serve(ctx, in, out)
}

func (s *StdioServer) handleRequest(ctx context.Context, msg mcpwire.Message, responder *StdioResponder) {
	token := extractProgressToken(msg)
	var notify func(ProgressUpdate)
	if len(token) > 0 {
		notify = func(update ProgressUpdate) {
			if raw, ok := marshalProgressNotification(token, update); ok {
				responder.EnqueueProgress(raw)
			}
		}
	}
	reply, fault := s.core.handleMessage(ctx, msg, notify)
	responder.Complete(reply, fault)
}
