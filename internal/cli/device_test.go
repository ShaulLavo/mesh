package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"golang.org/x/crypto/ssh"
)

func TestDeviceApprovalJSONReportsManagedPolicyChange(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	actor, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []bool{true, false} {
		var output bytes.Buffer
		command := deviceCommand()
		command.SetArgs([]string{"approve", "--allow-root", "--json", "--", actor.ID})
		command.SetOut(&output)
		if err := command.ExecuteContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Changed bool `json:"changed"`
		}
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Changed != expected {
			t.Fatalf("approval changed=%v, want %v", result.Changed, expected)
		}
	}
}

func TestInstallerPublicKeyApprovalPreservesIdempotentGrant(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	actor, private, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
	for _, value := range []string{"invalid", "command=\"restricted\" " + key, key + "\n" + key} {
		if _, err := deviceGrantIdentity(value, true); err == nil {
			t.Fatal("installer accepted malformed, restricted, or multiple public keys")
		}
	}
	for range 2 {
		var output bytes.Buffer
		command := deviceCommand()
		command.SetArgs([]string{"approve", "--allow-root", "--public-key", "--", key + " fixture-comment"})
		command.SetOut(&output)
		command.SetErr(&output)
		if err := command.ExecuteContext(t.Context()); err != nil {
			t.Fatalf("installer approval failed: %v", err)
		}
	}
	current, ok := identity.BindIdentity(state, actor.ID)
	if !ok {
		t.Fatal("installer did not approve the key identity")
	}
	if err := identity.ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if !current() {
		t.Fatal("semantic idempotent approval retired the installer grant")
	}
}

func TestDeviceCommandsPublishSuccessfulIdentityAndGrant(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	host, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"device", "identity"}, {"device", "approve", "--allow-root", host.ID}, {"device", "revoke", host.ID}} {
		output, _, err := executeCommand(t, Dependencies{}, args...)
		if err != nil {
			t.Fatalf("%v failed: %v (%s)", args, err, output)
		}
		if output == "" || strings.Contains(output, "%!w") {
			t.Fatalf("invalid device result: %q", output)
		}
	}
	if identity.GrantedIdentity(state, host.ID) {
		t.Fatal("device revoke left its grant")
	}
}
