// Package mcpframe embeds MCP messages in typed Mesh control frames.
package mcpframe

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"

	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
)

const (
	Forward = "mcp.forward"
	Message = "mcp.message"
	Result  = "mcp.result"
)

// Encode builds an H3 control payload and checks both wire limits.
func Encode(kind string, raw json.RawMessage) (protocol.Frame, error) {
	if _, err := validate(kind, raw); err != nil {
		return protocol.Frame{}, err
	}
	payload := make([]byte, 0, len(raw)+len(kind)+23)
	payload = append(payload, `{"type":"`...)
	payload = append(payload, kind...)
	payload = append(payload, `","message":`...)
	payload = append(payload, raw...)
	payload = append(payload, '}')
	if len(payload) > protocol.MaxPayload {
		return protocol.Frame{}, protocol.ErrPayloadTooLarge
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: payload}, nil
}

func Decode(frame protocol.Frame) (string, mcpwire.Message, error) {
	if len(frame.Payload) > protocol.MaxPayload {
		return "", mcpwire.Message{}, protocol.ErrPayloadTooLarge
	}
	if frame.Kind != protocol.KindControl || !jsontext.Value(frame.Payload).IsValid() {
		return "", mcpwire.Message{}, fmt.Errorf("invalid MCP control frame")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		return "", mcpwire.Message{}, fmt.Errorf("decode MCP control frame: %w", err)
	}
	var kind string
	if err := json.Unmarshal(payload["type"], &kind); err != nil || kind == "" || payload["message"] == nil {
		return "", mcpwire.Message{}, fmt.Errorf("missing MCP control frame type or message")
	}
	message, err := validate(kind, payload["message"])
	return kind, message, err
}

func validate(kind string, raw json.RawMessage) (mcpwire.Message, error) {
	if len(raw) > mcpwire.MaxMessage {
		return mcpwire.Message{}, mcpwire.ErrTooLarge
	}
	message, fault := mcpwire.Parse(raw)
	if fault != nil {
		return mcpwire.Message{}, fault
	}
	if (kind == Forward && message.Kind() == mcpwire.Request) ||
		(kind == Message && message.Kind() == mcpwire.Notification) ||
		(kind == Result && message.Kind() == mcpwire.Response) {
		return message, nil
	}
	return mcpwire.Message{}, fmt.Errorf("MCP message kind does not match control frame %q", kind)
}
