package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"golang.org/x/crypto/ssh"
)

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
	for _, args := range [][]string{{"identity"}, {"approve", "--allow-root", "--", host.ID}, {"revoke", "--", host.ID}} {
		var output bytes.Buffer
		command := deviceCommand()
		command.SetArgs(args)
		command.SetOut(&output)
		command.SetErr(&output)
		if err := command.ExecuteContext(t.Context()); err != nil {
			t.Fatalf("%v failed: %v (%s)", args, err, output.String())
		}
		if output.Len() == 0 || strings.Contains(output.String(), "%!w") {
			t.Fatalf("invalid device result: %q", output.String())
		}
	}
	if identity.GrantedIdentity(state, host.ID) {
		t.Fatal("device revoke left its grant")
	}
}
