package mcpnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

// dispatcher owns the transport-independent native MCP method and tool
// dispatch shared by the stdio (H1) and loopback HTTP (H6) frontends.
type dispatcher struct {
	registry   *Registry
	controls   Controls
	serverInfo json.RawMessage
}

func newDispatcher(opts ServerOptions) (*dispatcher, error) {
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
	return &dispatcher{
		registry:   registry,
		controls:   opts.Controls,
		serverInfo: serverInfo,
	}, nil
}

func (d *dispatcher) handleMessage(ctx context.Context, msg mcpwire.Message, notify func(ProgressUpdate)) ([]byte, *mcpwire.Fault) {
	switch msg.Method() {
	case mcpwire.MethodDiscover:
		return d.completeResult(msg, discoverPayload(), defaultDiscoverTTLMS)
	case mcpwire.MethodListTools:
		return d.handleListTools(msg)
	case mcpwire.MethodCallTool:
		return d.handleCallTool(ctx, msg, notify)
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

func (d *dispatcher) handleListTools(msg mcpwire.Message) ([]byte, *mcpwire.Fault) {
	var params struct {
		Cursor string `json:"cursor"`
	}
	if len(msg.Params()) > 0 {
		_ = json.Unmarshal(msg.Params(), &params)
	}
	if params.Cursor != "" {
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: mcpwire.InvalidParams, Message: "Invalid pagination cursor"}
	}
	raw, err := json.Marshal(map[string]any{"tools": d.registry.Definitions()})
	if err != nil {
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: meshRPCErrorCode, Message: err.Error()}
	}
	return d.completeResult(msg, raw, defaultDiscoverTTLMS)
}

func (d *dispatcher) handleCallTool(ctx context.Context, msg mcpwire.Message, notify func(ProgressUpdate)) ([]byte, *mcpwire.Fault) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params(), &params); err != nil {
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: mcpwire.InvalidParams, Message: "Invalid tools/call parameters"}
	}
	spec, ok := d.registry.Lookup(params.Name)
	if !ok {
		return nil, &mcpwire.Fault{ID: msg.ID(), Code: mcpwire.InvalidParams, Message: fmt.Sprintf("Unknown tool %q", params.Name)}
	}
	outcome, err := spec.Handler(ctx, d.controls, params.Arguments, notify)
	if err != nil {
		return d.toolHandlerError(msg, spec.Operation, err)
	}
	resultRaw, err := buildToolResult(outcome, spec.Operation)
	if err != nil {
		return nil, meshFault(msg.ID(), agentapi.InternalError, false, err.Error())
	}
	return d.completeResult(msg, resultRaw, 0)
}

func (d *dispatcher) toolHandlerError(msg mcpwire.Message, operation string, err error) ([]byte, *mcpwire.Fault) {
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
	return d.completeResult(msg, raw, 0)
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

func (d *dispatcher) completeResult(req mcpwire.Message, result json.RawMessage, ttlMS int64) ([]byte, *mcpwire.Fault) {
	reply, err := mcpwire.Complete(req, result, d.serverInfo, ttlMS)
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

// encodeBoundedFault encodes a JSON-RPC error within maxBytes (and
// mcpwire.MaxMessage), falling back to a null ID if the original request ID
// pushes the error reply over the byte limit.
func encodeBoundedFault(fault *mcpwire.Fault, maxBytes int) []byte {
	if fault == nil {
		return nil
	}
	limit := mcpwire.MaxMessage
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	if raw, err := fault.Reply(); err == nil && len(raw) <= limit {
		return raw
	}
	nullID := &mcpwire.Fault{
		Code:    fault.Code,
		Message: fault.Message,
		Data:    fault.Data,
	}
	if raw, err := nullID.Reply(); err == nil && len(raw) <= limit {
		return raw
	}
	minimal := &mcpwire.Fault{
		Code:    fault.Code,
		Message: agentapi.CleanMessage(fault.Message),
	}
	raw, _ := minimal.Reply()
	return raw
}

func prepareTerminalFrames(id mcpwire.ID, reply []byte, fault *mcpwire.Fault) (primary, fallback []byte, status int) {
	fallback = encodeBoundedFault(meshFault(id, agentapi.ResourceLimit, true, "Frontend output buffer limit exceeded."), reservedErrorBytes)
	if fault != nil {
		primary = encodeBoundedFault(fault, mcpwire.MaxMessage)
		return primary, fallback, faultHTTPStatus(fault.Code)
	}
	if len(reply) > mcpwire.MaxMessage {
		primary = encodeBoundedFault(meshFault(id, agentapi.MCPMessageTooLarge, false, "MCP reply exceeds the 3.5 MiB message limit."), mcpwire.MaxMessage)
		return primary, fallback, http.StatusOK
	}
	return reply, fallback, http.StatusOK
}

func faultHTTPStatus(code int) int {
	switch code {
	case mcpwire.MethodNotFound:
		return http.StatusNotFound
	case mcpwire.HeaderMismatch, mcpwire.MissingCapability, mcpwire.UnsupportedVersion,
		mcpwire.ParseError, mcpwire.InvalidRequest, mcpwire.InvalidParams:
		return http.StatusBadRequest
	default:
		return http.StatusOK
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

func marshalProgressNotification(token json.RawMessage, update ProgressUpdate) ([]byte, bool) {
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
		return nil, false
	}
	return raw, true
}
