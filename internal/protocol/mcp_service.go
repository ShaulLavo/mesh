package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Hosted MCP controls (#319). They use their own payload type so hosted
// service fields stay out of the shared Control struct. mcp.forward is in
// internal/protocol/mcpframe.
const (
	TypeMCPCatalog        = "mcp.catalog"
	TypeMCPServiceUpsert  = "mcp.service.upsert"
	TypeMCPServiceDelete  = "mcp.service.delete"
	TypeMCPServiceStart   = "mcp.service.start"
	TypeMCPServiceStop    = "mcp.service.stop"
	TypeMCPServiceRestart = "mcp.service.restart"
	TypeMCPServiceLogs    = "mcp.service.logs"
	TypeMCPServiceLease   = "mcp.service.lease"
	TypeMCPServiceResult  = "mcp.service.result"
)

// IsMCPControl reports whether kind is a hosted MCP control, including
// mcp.forward.
func IsMCPControl(kind string) bool { return strings.HasPrefix(kind, "mcp.") }

var mcpControlPrefix = []byte(`{"type":"mcp.`)

// MCPControlType recognizes an MCP control frame before the generic control
// decoder sees it: mcp.forward embeds a raw JSON message that the shared
// Control struct cannot hold. Mesh encoders always write the type first, so
// other frames are rejected by the prefix without a JSON decode.
func MCPControlType(frame Frame) (string, bool) {
	if frame.Kind != KindControl || !bytes.HasPrefix(frame.Payload, mcpControlPrefix) {
		return "", false
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame.Payload, &head) != nil || !IsMCPControl(head.Type) {
		return "", false
	}
	return head.Type, true
}

// MCPServiceRequest is one hosted service management or catalog request.
type MCPServiceRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	// Service is a service ID or name on the destination host.
	Service string `json:"service,omitempty"`
	// Instance selects an instance by ID; New asks for another isolated one.
	Instance string `json:"instance,omitempty"`
	New      bool   `json:"new,omitempty"`
	// AppCredential is the application caller's private credential. The host
	// derives the caller from it and never stores or echoes it.
	AppCredential string `json:"appCredential,omitempty"`
	// Definition is the submitted service definition for upsert.
	Definition       json.RawMessage `json:"definition,omitempty"`
	ExpectedRevision int64           `json:"expectedRevision,omitempty"`
	LeaseID          string          `json:"leaseId,omitempty"`
	LeaseTTLMillis   int64           `json:"leaseTtlMillis,omitempty"`
	Job              string          `json:"job,omitempty"`
	MaxBytes         int             `json:"maxBytes,omitempty"`
}

// MCPServiceResponse carries the result as JSON owned by internal/mcphost.
type MCPServiceResponse struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId"`
	Result    json.RawMessage `json:"result,omitempty"`
	ErrorCode string          `json:"errorCode,omitempty"`
	Message   string          `json:"message,omitempty"`
	Retryable bool            `json:"retryable,omitempty"`
}

func DecodeMCPServiceRequest(payload []byte) (MCPServiceRequest, error) {
	var req MCPServiceRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return req, fmt.Errorf("protocol: decode MCP control: %w", err)
	}
	if !IsMCPControl(req.Type) || req.RequestID == "" {
		return req, fmt.Errorf("protocol: MCP control needs an mcp type and a request ID")
	}
	return req, nil
}

// EncodeMCP builds one control frame for an MCP request or response.
func EncodeMCP(value any) (Frame, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return Frame{}, fmt.Errorf("protocol: encode MCP control: %w", err)
	}
	if len(payload) > MaxPayload {
		return Frame{}, ErrPayloadTooLarge
	}
	return Frame{Kind: KindControl, Payload: payload}, nil
}
