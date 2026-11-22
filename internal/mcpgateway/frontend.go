package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/mcpwire"
)

const meshRPCErrorCode = -31000

// FrontendOptions configures the gateway's stdio frontend.
type FrontendOptions struct {
	Stderr     io.Writer
	OnShutdown func()
}

// Frontend serves a Gateway over stdio on the shared single-reader,
// single-writer engine of `mesh mcp` and `mesh mcp connect`.
type Frontend struct {
	gateway *Gateway
	engine  *mcpnative.StdioEngine
}

// NewFrontend builds the stdio frontend for g.
func NewFrontend(g *Gateway, opts FrontendOptions) (*Frontend, error) {
	f := &Frontend{gateway: g}
	engine, err := mcpnative.NewStdioEngine(mcpnative.StdioEngineOptions{
		Methods:        Methods,
		IsStreamMethod: func(method string) bool { return method == mcpwire.MethodListen },
		Handler:        f.handle,
		CapacityFault: func(msg mcpwire.Message) *mcpwire.Fault {
			return meshFault(msg.ID(), agentapi.ResourceLimit, true, "", g.opts.Profile, "Frontend request capacity reached; retry after active requests finish.")
		},
		LogPrefix:  "mesh mcp",
		Stderr:     opts.Stderr,
		OnShutdown: opts.OnShutdown,
	})
	if err != nil {
		return nil, fmt.Errorf("create gateway stdio engine: %w", err)
	}
	f.engine = engine
	return f, nil
}

// Serve runs until stdin ends, ctx is cancelled or stdout fails.
func (f *Frontend) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if err := f.engine.Serve(ctx, in, out); err != nil {
		return fmt.Errorf("serve MCP gateway stdio: %w", err)
	}
	return nil
}

func (f *Frontend) handle(ctx context.Context, msg mcpwire.Message, responder *mcpnative.StdioResponder) {
	var terminal []byte
	err := f.gateway.Forward(ctx, msg, responder.Budget(), func(raw []byte, last bool) error {
		if last {
			terminal = raw
			return nil
		}
		if isProgress(raw) {
			responder.EnqueueProgress(raw)
			return nil
		}
		if err := responder.EnqueueNotification(raw); err != nil {
			return fmt.Errorf("enqueue MCP notification: %w", err)
		}
		return nil
	})
	if err == nil {
		responder.Complete(terminal, nil)
		return
	}
	if ctx.Err() != nil {
		// A cancelled request gets no reply.
		responder.Complete(nil, nil)
		return
	}
	responder.Complete(f.errorReply(msg, err))
}

// errorReply reports a failure outside an upstream reply: a tool execution
// error inside tools/call, and a -31000 Mesh error elsewhere (section 2.4).
func (f *Frontend) errorReply(msg mcpwire.Message, err error) ([]byte, *mcpwire.Fault) {
	code, retryable, host, service, message := agentapi.StateUnavailable, true, "", f.gateway.opts.Profile, agentapi.CleanMessage(err.Error())
	var rf *mcpconnect.RouteFailure
	if errors.As(err, &rf) {
		code, retryable, host, message = rf.Code, rf.Retryable, rf.Host, rf.Error()
		if rf.Service != "" {
			service = rf.Service
		}
	} else if errors.Is(err, mcpnative.ErrStreamOverfull) {
		code, retryable, message = agentapi.ResourceLimit, true, "Frontend stream buffer limit reached; listen again."
	}
	if msg.Method() == mcpwire.MethodCallTool {
		if reply, encodeErr := mcpconnect.ToolErrorResult(msg.ID(), code, retryable, host, service, message, f.gateway.opts.ServerInfo); encodeErr == nil {
			return reply, nil
		}
	}
	return nil, meshFault(msg.ID(), code, retryable, host, service, message)
}

func meshFault(id mcpwire.ID, code agentapi.Code, retryable bool, host, service, message string) *mcpwire.Fault {
	data := map[string]any{"meshCode": string(code), "retryable": retryable}
	if host != "" {
		data["host"] = host
	}
	if service != "" {
		data["service"] = service
	}
	rpc := meshRPCErrorCode
	if code == agentapi.MCPStateInvalid {
		rpc = mcpwire.InvalidParams
	}
	return &mcpwire.Fault{ID: id, Code: rpc, Message: agentapi.CleanMessage(message), Data: data}
}

func isProgress(raw []byte) bool {
	var header struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(raw, &header) == nil && header.Method == "notifications/progress"
}
