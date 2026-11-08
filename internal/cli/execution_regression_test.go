package cli

import (
	"encoding/json"
	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"testing"
)

func TestExecutionRegressionNewExecutionCodesReachPublicEnvelope(t *testing.T) {
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
	for _, test := range []struct{ command, code string }{{"logs", "logs.cursor_expired"}, {"logs", "logs.cursor_invalid"}, {"wait", "result.expired"}, {"signal", "session.instance_mismatch"}, {"logs", "usage.invalid"}} {
		t.Run(test.code, func(t *testing.T) { reviewPublicCode(t, host, test.command, test.code) })
	}
}
func reviewPublicCode(t *testing.T, host HostRecord, command, code string) {
	t.Helper()
	passive := serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, ErrorCode: code, Message: "expected diagnostic"}
	})
	args := []string{"session", command, "--host", host.ID, "7K3D", "--instance", "0123456789abcdef0123456789abcdef", "--json"}
	if command == "signal" {
		args = append(args, "--signal", "term")
	}
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: passive}, args...)
	var envelope agentapi.Envelope
	if decodeErr := json.Unmarshal([]byte(stdout), &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if envelope.Operation != "sessions."+command || envelope.Status != agentapi.StatusError || len(envelope.Errors) != 1 || string(envelope.Errors[0].Code) != code {
		t.Fatalf("unexpected envelope: %s", stdout)
	}
	status, ok := StatusCode(err)
	if !ok || status != agentapi.Codes[agentapi.Code(code)].Exit || stderr != "" {
		t.Fatalf("wrong public exit: %v %q", err, stderr)
	}
}
