package mcpframe

import (
	"encoding/json"
	"fmt"

	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
)

// Route selects the hosted service instance an mcp.forward goes to. It
// travels beside the MCP message, never inside it.
type Route struct {
	Service       string `json:"service"`
	Instance      string `json:"instance,omitempty"`
	New           bool   `json:"new,omitempty"`
	AppCredential string `json:"appCredential,omitempty"`
}

// EncodeForward builds the opening frame of an H3 stream.
func EncodeForward(route Route, raw json.RawMessage) (protocol.Frame, error) {
	if _, err := validate(Forward, raw); err != nil {
		return protocol.Frame{}, err
	}
	encoded, err := json.Marshal(route)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("encode MCP route: %w", err)
	}
	payload := make([]byte, 0, len(raw)+len(encoded)+48)
	payload = append(payload, `{"type":"`+Forward+`","route":`...)
	payload = append(payload, encoded...)
	payload = append(payload, `,"message":`...)
	payload = append(payload, raw...)
	payload = append(payload, '}')
	if len(payload) > protocol.MaxPayload {
		return protocol.Frame{}, protocol.ErrPayloadTooLarge
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: payload}, nil
}

// DecodeForward reads the opening frame of an H3 stream.
func DecodeForward(frame protocol.Frame) (Route, mcpwire.Message, error) {
	kind, message, err := Decode(frame)
	if err != nil {
		return Route{}, message, err
	}
	if kind != Forward {
		return Route{}, message, fmt.Errorf("expected %s, got %s", Forward, kind)
	}
	var envelope struct {
		Route *Route `json:"route"`
	}
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil || envelope.Route == nil || envelope.Route.Service == "" {
		return Route{}, message, fmt.Errorf("%s needs a route naming a service", Forward)
	}
	return *envelope.Route, message, nil
}
