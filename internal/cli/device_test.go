package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
)

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
