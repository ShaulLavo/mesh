package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
)

func TestFleetProfileStoppedDaemonWriteAndRetry(t *testing.T) {
	state, config := t.TempDir(), t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(input, []byte(`{"schemaVersion":1,"instructions":"fleet notes"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		out, stderr, err := executeCommand(t, Dependencies{}, "profile", "set", "--fleet", "--file", input, "--expected-revision", "0", "--json")
		if err != nil || stderr != "" {
			t.Fatalf("fleet set attempt %d: %s %s %v", attempt, out, stderr, err)
		}
		var value struct {
			Data agentapi.ProfileWrite `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &value); err != nil {
			t.Fatal(err)
		}
		if value.Data.Revision != 1 || !value.Data.Committed || !value.Data.Queued || value.Data.FleetSaved || value.Data.Changed != (attempt == 0) {
			t.Fatalf("wrong durable result: %s", out)
		}
	}
	out, stderr, err := executeCommand(t, Dependencies{}, "profile", "status", "--fleet", "--json")
	if err != nil || stderr != "" {
		t.Fatal(out, stderr, err)
	}
	var value struct {
		Data agentapi.FleetStatus `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &value); err != nil {
		t.Fatal(err)
	}
	if value.Data.Replica.State != "valid" || value.Data.FleetSaved || value.Data.Members == nil {
		t.Fatal(out)
	}
}
