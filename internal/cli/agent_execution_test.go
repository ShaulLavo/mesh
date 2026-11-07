package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestAgentExecutionUsageDoesNotInitializeState(t *testing.T) {
	absent := t.TempDir() + "/absent"
	t.Setenv("MESH_STATE_DIR", absent)
	t.Setenv("MESH_CONFIG_DIR", absent)
	cases := [][]string{{"create", "--host", "host", "--json"}, {"create", "--host", "host", "--kind", "pipe", "--cwd", "/work", "--request-key", "k", "--json", "--", "/bin/true"}, {"request", "--host", "host", "--request-key", "has space", "--json"}, {"request", "--json"}, {"create", "--json", "--host", "host", "--request-key", "k", "--cwd", "/work", "--kind", "pty", "/bin/true"}}
	for _, args := range cases {
		operation := "sessions.request"
		if args[0] == "create" {
			operation = "sessions.create"
		}
		stdout, stderr, err := executeCommand(t, Dependencies{}, append([]string{"session"}, args...)...)
		assertAgentSessionFailure(t, stdout, stderr, err, operation, "usage.invalid", 2)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("usage created state")
	}
}
func TestAgentExecutionUsesOnlineOnlyDialAndPreservesPartialReceipt(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	id := argumentIdentities(t)[1]
	host := HostRecord{ID: id, MeshIdentity: id, Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	active := func(context.Context, HostRecord) (transport.Conn, error) {
		t.Error("active/wake dial used")
		return nil, context.Canceled
	}
	data := &protocol.ExecutionReceipt{HostID: id, SessionID: "7K3D", Instance: "0123456789abcdef0123456789abcdef", RequestKey: "job", KeyScope: "device", Kind: "pty", Phase: "uncertain", Acceptance: "durable", LaunchOutcome: "unknown", Publication: "pending", Removal: "present"}
	passive := serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: "request.outcome_unknown", Message: "launch uncertain", ExecutionReceipt: data}
	})
	stdout, stderr, err := executeCommand(t, Dependencies{DialHost: active, DialPassive: passive}, "session", "request", "--host", id, "--request-key", "job", "--json")
	var result struct {
		Status string                    `json:"status"`
		Data   agentapi.ExecutionReceipt `json:"data"`
		Errors []agentapi.Error          `json:"errors"`
	}
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	code, ok := StatusCode(err)
	if !ok || code != 3 || stderr != "" || result.Status != "partial" || result.Data.Instance != data.Instance || result.Errors[0].Code != agentapi.RequestOutcomeUnknown {
		t.Fatalf("partial receipt lost: %s %s %v", stdout, stderr, err)
	}
}
func TestAgentExecutionDecodedGoldens(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	until := at.Add(30 * 24 * time.Hour)
	code := 7
	wire := &protocol.ExecutionReceipt{HostID: "host", SessionID: "7K3D", Instance: "0123456789abcdef0123456789abcdef", RequestKey: "job", KeyScope: "local-account", Kind: "pty", Phase: "terminal", Acceptance: "durable", LaunchOutcome: "started", Publication: "published", Removal: "present", ReceiptRetainUntil: &until, Result: &protocol.ExecutionResult{Termination: protocol.ExecutionTermination{Kind: "exit", Code: &code}, EndedAt: &at, Capture: "not_recorded"}}
	data, err := agentExecutionReceipt(wire, "host", "job")
	if err != nil {
		t.Fatal(err)
	}
	checkAgentSessionGolden[agentapi.ExecutionReceipt](t, "sessions.create.terminal", "sessions.create", agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
	wire.Phase = "uncertain"
	wire.Result = nil
	wire.ReceiptRetainUntil = nil
	wire.LaunchOutcome = "unknown"
	wire.Publication = "pending"
	data, err = agentExecutionReceipt(wire, "host", "job")
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := agentapi.Fail(agentapi.RequestOutcomeUnknown, "sessions.request", "host", "Launch outcome is unknown.").Err
	checkAgentSessionGolden[agentapi.ExecutionReceipt](t, "sessions.request.uncertain", "sessions.request", agentapi.Envelope{Status: agentapi.StatusPartial, Data: data, Errors: []agentapi.Error{diagnostic}})
}
