package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestFleetProfilePartialPublicationAndRepeatedRepair(t *testing.T) {
	state, config := t.TempDir(), t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(state, "fleet")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	obstruction := filepath.Join(directory, "outbox.json")
	if err := os.Mkdir(obstruction, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(input, []byte(`{"schemaVersion":1,"instructions":"durable notes"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		out, stderr, err := executeCommand(t, Dependencies{}, "profile", "set", "--fleet", "--file", input, "--expected-revision", "0", "--json")
		if code, ok := StatusCode(err); !ok || code != 3 {
			t.Fatal(out, stderr, err)
		}
		var result struct {
			Status string                `json:"status"`
			Data   agentapi.ProfileWrite `json:"data"`
			Errors []agentapi.Error      `json:"errors"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != "partial" || !result.Data.Committed || result.Data.Queued || result.Data.Publication != "replica-durable" || result.Data.Revision != 1 || result.Data.Members == nil || len(result.Errors) != 1 || result.Errors[0].Code != agentapi.StateUnavailable {
			t.Fatal(out)
		}
	}
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := executeCommand(t, Dependencies{}, "profile", "set", "--fleet", "--file", input, "--expected-revision", "0", "--json")
	if err != nil || stderr != "" {
		t.Fatal(out, stderr, err)
	}
	var result struct {
		Data agentapi.ProfileWrite `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Revision != 1 || result.Data.Changed || !result.Data.Queued {
		t.Fatal(out)
	}
}

func TestFleetProfileDecodedGoldens(t *testing.T) {
	state, config := t.TempDir(), t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	result, _, err := runProfile(t, bytes.NewBufferString(`{"schemaVersion":1,"instructions":"fleet notes"}`), "profile", "set", "--fleet", "--file", "-", "--expected-revision", "0", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var write agentapi.ProfileWrite
	decodeProfileData(t, result.Data, &write)
	if write.Publication != "complete" || !write.Committed || !write.Queued || write.Revision != 1 || write.FleetSaved || len(write.Members) != 0 {
		t.Fatal(write)
	}
	write.Digest = "DIGEST"
	result.Data = write
	assertProfileGolden(t, "fleet.profile.set", result)
	result, _, err = runProfile(t, noProfileInput{t}, "profile", "status", "--fleet", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var status agentapi.FleetStatus
	decodeProfileData(t, result.Data, &status)
	if status.Replica.State != "valid" || status.Replica.Author == "" || status.Replica.Profile.Instructions == nil {
		t.Fatal(status)
	}
	status.Replica.Digest, status.Replica.Author = "DIGEST", "AUTHOR"
	status.Replica.UpdatedAt, status.Replica.ReceivedAt = nil, nil
	result.Data = status
	assertProfileGolden(t, "fleet.profile.status", result)
}
func TestFleetProfileUsesFleetLimits(t *testing.T) {
	state, config := t.TempDir(), t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"schemaVersion": 1, "instructions": strings.Repeat("x", 64<<10)})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := runProfile(t, bytes.NewReader(input), "profile", "set", "--fleet", "--file", "-", "--expected-revision", "0", "--json")
	if err != nil {
		t.Fatal(result, err)
	}
	result, _, err = runProfile(t, noProfileInput{t}, "profile", "show", "--fleet", "--json")
	if err != nil {
		t.Fatal(result, err)
	}
	var section agentapi.FleetSection
	decodeProfileData(t, result.Data, &section)
	if section.Profile.InstructionsBytes != 64<<10 {
		t.Fatal(section.Profile.InstructionsBytes)
	}
}
