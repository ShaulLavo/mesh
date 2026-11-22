package daemon

import (
	"encoding/json"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
	"github.com/shaul/mesh/internal/update"
)

func mcpFrames(t *testing.T) []protocol.Frame {
	t.Helper()
	var frames []protocol.Frame
	for _, kind := range []string{protocol.TypeMCPCatalog, protocol.TypeMCPServiceUpsert, protocol.TypeMCPServiceDelete, protocol.TypeMCPServiceStart,
		protocol.TypeMCPServiceStop, protocol.TypeMCPServiceRestart, protocol.TypeMCPServiceLogs, protocol.TypeMCPServiceLease} {
		frame, err := protocol.EncodeMCP(protocol.MCPServiceRequest{Type: kind, RequestID: "denied", Service: "fixture", Definition: json.RawMessage(`{"name":"x"}`)})
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	forward, err := mcpframe.EncodeForward(mcpframe.Route{Service: "fixture"}, json.RawMessage(mcpToolCall(`1`, "whoami", `{}`)))
	if err != nil {
		t.Fatal(err)
	}
	return append(frames, forward)
}

// Scoped metadata keys reach none of the hosted MCP controls over real TLS.
func TestScopedPeersCannotReachMCP(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	for _, frame := range mcpFrames(t) {
		assertScopedDenied(t, f, frame)
	}
	if f.launches.Load() != 0 || f.workers.calls.Load() != 0 {
		t.Fatal("a denied MCP control reached an owner")
	}
}

// A signed updater's key admits only update controls, never MCP.
func TestUpdateCarrierCannotReachMCP(t *testing.T) {
	state := t.TempDir()
	actor, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Trust(t.Context(), state, actor.ID, true); err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	grant := auth.Bind(actor.ID)
	for _, frame := range mcpFrames(t) {
		if grant.Allows(frame) {
			t.Fatalf("update carrier admitted %s", frame.Payload)
		}
	}
}
