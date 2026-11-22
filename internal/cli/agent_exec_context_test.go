package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/protocol"
)

func TestContextHandleMustNameTheDialedHost(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	if got, err := contextOnHost("sessions.create", "host-a", protocol.ContextHandle("host-a", id)); err != nil || got != id {
		t.Fatalf("matching handle = %s, %v", got, err)
	}
	if got, err := contextOnHost("sessions.create", "host-a", id); err != nil || got != id {
		t.Fatalf("bare ID = %s, %v", got, err)
	}
	config := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, err := appcaller.Create(config, "agent", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, request := range []protocol.Control{
		{Type: protocol.TypeExecutionCreate},
		{Type: protocol.TypeExecutionRequest},
		{Type: protocol.TypeExecutionKill, SessionID: "7K3D", Instance: id},
	} {
		options := agentExecutionOptions{identity: "agent", context: protocol.ContextHandle("host-b", id)}
		err := attachApplicationCaller("sessions.create", "host-a", options, &request)
		if agentFailureCode(err) != agentapi.HostIdentityMismatch || request.Caller != "" {
			t.Fatalf("%s with another host's handle = %v, caller attached %v", request.Type, err, request.Caller != "")
		}
	}
}

func agentFailureCode(err error) agentapi.Code {
	var failure *agentapi.Failure
	if errors.As(err, &failure) {
		return failure.Err.Code
	}
	return ""
}
