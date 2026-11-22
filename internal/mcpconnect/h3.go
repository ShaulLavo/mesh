package mcpconnect

import (
	"context"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
	"github.com/shaul/mesh/internal/transport"
)

// ForwardMethods are the MCP 2026-07-28 methods carried across H3.
var ForwardMethods = mcpwire.Methods{
	mcpwire.MethodDiscover:              {},
	mcpwire.MethodListTools:             {},
	mcpwire.MethodCallTool:              {},
	mcpwire.MethodListPrompts:           {},
	mcpwire.MethodGetPrompt:             {},
	mcpwire.MethodListResources:         {},
	mcpwire.MethodListResourceTemplates: {},
	mcpwire.MethodReadResource:          {},
	mcpwire.MethodComplete:              {},
	mcpwire.MethodListen:                {},
}

// Stream represents one open, pinned, authenticated Mesh control connection
// allocated for a single H3 forward exchange.
type Stream interface {
	Conn() transport.Conn
	HostID() string
	MCPHostingSupported() bool
	Close()
}

// Opener allocates a connection under the host pool's short-control (listen=false)
// or stream (listen=true) concurrency budget.
type Opener func(ctx context.Context, listen bool) (Stream, error)

// ErrorClassifier maps transport/daemon errors to stable agentapi codes.
type ErrorClassifier func(err error) (agentapi.Code, bool)

// Forwarder sends one validated MCP request over H3 and delivers correlated
// notifications followed by one terminal response.
type Forwarder interface {
	Forward(ctx context.Context, req mcpwire.Message, emit func(raw []byte, terminal bool) error) error
}

// RouteFailure is a structured Mesh failure outside a normal upstream reply.
type RouteFailure struct {
	Code      agentapi.Code
	Retryable bool
	Host      string
	Service   string
	Message   string
	Err       error
}

func (f *RouteFailure) Error() string {
	if f.Message != "" {
		return f.Message
	}
	if f.Err != nil {
		return f.Err.Error()
	}
	return string(f.Code)
}

func (f *RouteFailure) Unwrap() error { return f.Err }

// H3Forwarder forwards MCP requests over authenticated Mesh H3 frames.
type H3Forwarder struct {
	Open     Opener
	Route    mcpframe.Route
	HostID   string
	Classify ErrorClassifier
}

// Forward runs one H3 exchange, retrying discovery and unconditional list
// requests once on transport failure before any downstream frame is emitted.
func (f *H3Forwarder) Forward(ctx context.Context, req mcpwire.Message, emit func(raw []byte, terminal bool) error) error {
	if f.Open == nil {
		return f.fail(agentapi.StateUnavailable, false, errors.New("H3 opener is not configured"))
	}
	frame, err := mcpframe.EncodeForward(f.Route, req.Raw())
	if err != nil {
		return f.encodeError(err)
	}
	attempts := 1
	if retryableRead(req) {
		attempts = 2
	}
	var lastErr error
	for attempt := range attempts {
		emitted := false
		counted := func(raw []byte, terminal bool) error {
			emitted = true
			return emit(raw, terminal)
		}
		transportFailure, err := f.forwardOnce(ctx, req, frame, counted)
		if err == nil {
			return nil
		}
		lastErr = err
		if emitted || !transportFailure || ctx.Err() != nil || attempt+1 >= attempts {
			return err
		}
	}
	return lastErr
}

func (f *H3Forwarder) encodeError(err error) error {
	if errors.Is(err, mcpwire.ErrTooLarge) || errors.Is(err, protocol.ErrPayloadTooLarge) {
		return f.fail(agentapi.MCPMessageTooLarge, false, fmt.Errorf("MCP request exceeds the 3.5 MiB / 4 MiB H3 frame limit: %w", err))
	}
	return f.fail(agentapi.UsageInvalid, false, err)
}

func (f *H3Forwarder) forwardOnce(ctx context.Context, req mcpwire.Message, frame protocol.Frame, emit func(raw []byte, terminal bool) error) (bool, error) {
	stream, err := f.Open(ctx, req.Method() == mcpwire.MethodListen)
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("open H3 stream: %w", ctx.Err())
		}
		code, retryable := f.classify(err, agentapi.HostUnreachable, true)
		return retryable && code == agentapi.HostUnreachable, f.fail(code, retryable, err)
	}
	defer stream.Close()
	if !stream.MCPHostingSupported() {
		return false, f.fail(agentapi.ProtocolUnsupported, false, errors.New("target host does not support hosted MCP servers"))
	}
	stop := context.AfterFunc(ctx, stream.Close)
	defer stop()

	conn := stream.Conn()
	if err := conn.WriteFrame(frame); err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("write H3 forward: %w", ctx.Err())
		}
		code, retryable := f.classify(err, agentapi.HostUnreachable, true)
		return retryable, f.fail(code, retryable, fmt.Errorf("write H3 forward: %w", err))
	}
	return f.readExchange(ctx, conn, req, emit)
}

func (f *H3Forwarder) readExchange(ctx context.Context, conn transport.Conn, req mcpwire.Message, emit func(raw []byte, terminal bool) error) (bool, error) {
	exchange := mcpwire.NewExchange(req, mcpwire.MeshReply)
	for {
		reply, err := conn.ReadFrame()
		if err != nil {
			return f.handleReadError(ctx, exchange, req, err)
		}
		kind, msg, err := mcpframe.Decode(reply)
		if err != nil {
			return false, f.handleDecodeError(reply, err)
		}
		event, err := exchange.Accept(msg.Raw())
		if err != nil {
			return false, f.fail(agentapi.ServiceFailed, false, fmt.Errorf("validate H3 message: %w", err))
		}
		if (kind == mcpframe.Result) != event.Terminal {
			return false, f.fail(agentapi.ServiceFailed, false, fmt.Errorf("H3 frame kind %q does not match terminal=%t", kind, event.Terminal))
		}
		if err := emit(msg.Raw(), event.Terminal); err != nil {
			return false, err
		}
		if event.Terminal {
			return false, nil
		}
	}
}

func (f *H3Forwarder) handleReadError(ctx context.Context, exchange *mcpwire.Exchange, req mcpwire.Message, readErr error) (bool, error) {
	if ctx.Err() != nil {
		return false, fmt.Errorf("H3 stream cancelled: %w", ctx.Err())
	}
	if err := exchange.Finish(); errors.Is(err, mcpwire.ErrInterrupted) {
		if retryableRead(req) {
			return true, f.fail(agentapi.HostUnreachable, true, fmt.Errorf("H3 stream interrupted before reply: %w", readErr))
		}
		return false, f.fail(agentapi.RequestOutcomeUnknown, false, fmt.Errorf("H3 stream interrupted before terminal reply; outcome is unknown and was not retried: %w", readErr))
	}
	code, retryable := f.classify(readErr, agentapi.HostUnreachable, true)
	return retryable, f.fail(code, retryable, readErr)
}

func (f *H3Forwarder) handleDecodeError(frame protocol.Frame, decodeErr error) error {
	if ctrl, err := protocol.DecodeControl(frame.Payload); err == nil && ctrl.Type == protocol.TypeError {
		code, retryable := mapControlCode(ctrl.ErrorCode)
		return f.fail(code, retryable, fmt.Errorf("%s", ctrl.Message))
	}
	if errors.Is(decodeErr, mcpwire.ErrTooLarge) || errors.Is(decodeErr, protocol.ErrPayloadTooLarge) {
		return f.fail(agentapi.MCPMessageTooLarge, false, decodeErr)
	}
	return f.fail(agentapi.ServiceFailed, false, fmt.Errorf("decode H3 frame: %w", decodeErr))
}

func (f *H3Forwarder) classify(err error, fallback agentapi.Code, fallbackRetryable bool) (agentapi.Code, bool) {
	if f.Classify != nil {
		return f.Classify(err)
	}
	return fallback, fallbackRetryable
}

func (f *H3Forwarder) fail(code agentapi.Code, retryable bool, err error) *RouteFailure {
	return &RouteFailure{
		Code:      code,
		Retryable: retryable,
		Host:      f.HostID,
		Service:   f.Route.Service,
		Message:   agentapi.CleanMessage(err.Error()),
		Err:       err,
	}
}

func retryableRead(req mcpwire.Message) bool {
	if hasMember(req.Raw(), keyParams, "requestState") {
		return false
	}
	switch req.Method() {
	case mcpwire.MethodDiscover, mcpwire.MethodListTools, mcpwire.MethodListPrompts,
		mcpwire.MethodListResources, mcpwire.MethodListResourceTemplates:
		return true
	default:
		return false
	}
}

func mapControlCode(code string) (agentapi.Code, bool) {
	switch code {
	case "control.unknown", "protocol.unsupported":
		return agentapi.ProtocolUnsupported, false
	case "access.denied":
		return agentapi.AccessDenied, false
	case "service.not_found":
		return agentapi.ServiceNotFound, false
	case "service.incompatible":
		return agentapi.ServiceIncompatible, false
	case "service.unavailable":
		return agentapi.ServiceUnavailable, true
	case "resource.limit":
		return agentapi.ResourceLimit, true
	case "host.identity_mismatch":
		return agentapi.HostIdentityMismatch, false
	default:
		return agentapi.ServiceFailed, false
	}
}
