package mcpframe

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
)

const rawForward = `{"jsonrpc":"2.0","id":900719925474099312345,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"progressToken":"trace"},"vendor":{"x":"<unchanged>"}}}`

func TestMCPControlFrames(t *testing.T) {
	request := crossMCPFrame(t, Forward, rawForward)
	if fault := mcpwire.ValidateRequest(request, mcpwire.Methods{"tools/list": nil}); fault != nil {
		t.Fatal(fault)
	}
	exchange := mcpwire.NewExchange(request, mcpwire.MeshReply)
	notification := crossMCPFrame(t, Message, `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"trace","progress":1}}`)
	if event, err := exchange.Accept(notification.Raw()); err != nil || event.Terminal {
		t.Fatalf("notification: %+v, %v", event, err)
	}
	if err := exchange.Finish(); !errors.Is(err, mcpwire.ErrInterrupted) {
		t.Fatalf("close following a notification: %v", err)
	}
	terminal := crossMCPFrame(t, Result, `{"jsonrpc":"2.0","id":900719925474099312345,"result":{"resultType":"complete","tools":[],"ttlMs":0,"cacheScope":"private"}}`)
	if event, err := exchange.Accept(terminal.Raw()); err != nil || !event.Terminal || exchange.Finish() != nil {
		t.Fatalf("terminal: %+v, %v", event, err)
	}
	if _, err := exchange.Accept(terminal.Raw()); !errors.Is(err, mcpwire.ErrPeerMessage) {
		t.Fatalf("second terminal: %v", err)
	}
	badTerminal := strings.Replace(string(terminal.Raw()), "900719925474099312345", `"900719925474099312345"`, 1)
	otherID := crossMCPFrame(t, Result, badTerminal)
	if _, err := mcpwire.NewExchange(request, mcpwire.MeshReply).Accept(otherID.Raw()); !errors.Is(err, mcpwire.ErrPeerMessage) {
		t.Fatalf("wrong terminal ID: %v", err)
	}
}

func crossMCPFrame(t *testing.T, kind, raw string) mcpwire.Message {
	t.Helper()
	frame, err := Encode(kind, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := protocol.NewWriter(&wire).WriteControl(frame.Payload); err != nil {
		t.Fatal(err)
	}
	framed, err := protocol.NewReader(&wire).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	got, message, err := Decode(framed)
	if err != nil || got != kind || string(message.Raw()) != raw {
		t.Fatalf("wanted raw %s %s, got %s %s, %v", kind, raw, got, message.Raw(), err)
	}
	return message
}

func TestMCPFrameBounds(t *testing.T) {
	prefix := strings.TrimSuffix(rawForward, "}")
	// Padding is part of the MCP object and is measured before JSON decoding.
	makeRaw := func(size int) string {
		return prefix + strings.Repeat(" ", size-len(prefix)-1) + "}"
	}
	frame, err := Encode(Forward, json.RawMessage(makeRaw(mcpwire.MaxMessage)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := Encode(Forward, json.RawMessage(makeRaw(mcpwire.MaxMessage+1))); !errors.Is(err, mcpwire.ErrTooLarge) {
		t.Fatalf("inner MCP limit: %v", err)
	}
	for _, tc := range []struct {
		payload string
		want    error
	}{
		{`{"type":"mcp.forward","message":` + makeRaw(mcpwire.MaxMessage+1) + "}", mcpwire.ErrTooLarge},
		{`{"type":"mcp.forward","message":` + rawForward + strings.Repeat(" ", protocol.MaxPayload) + "}", protocol.ErrPayloadTooLarge},
		{`{"type":"mcp.forward","message":"e30="}`, nil},
		{`{"type":"mcp.forward","type":"mcp.message","message":` + rawForward + "}", nil},
		{`{"type":"mcp.result","message":` + rawForward + "}", nil},
	} {
		_, _, err := Decode(protocol.Frame{Kind: protocol.KindControl, Payload: []byte(tc.payload)})
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("unexpected error for payload of %d bytes: %v", len(tc.payload), err)
		}
	}
}
