package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
)

func TestProfileMissingIdentityNoncreation(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	config := filepath.Join(root, "config")
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	out, stderr, err := executeCommand(t, Dependencies{}, "profile", "show", "--json")
	var result agentapi.Envelope
	if decode := json.Unmarshal([]byte(out), &result); decode != nil {
		t.Fatalf("not one result: %q %v", out, decode)
	}
	if result.Operation != "profile.show" || len(result.Errors) != 1 || result.Errors[0].Code != agentapi.HostUninitialized || stderr != "" {
		t.Fatal(out, stderr, err)
	}
	for _, path := range []string{state, config} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("read created %s: %v", path, err)
		}
	}
}

func TestProfileStoppedDaemonLocalRoundTrip(t *testing.T) {
	state := t.TempDir()
	config := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(input, []byte(`{"schemaVersion":1,"instructions":"hello\nworld"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := executeCommand(t, Dependencies{}, "profile", "set", "--file", input, "--expected-revision", "0", "--json")
	if err != nil || stderr != "" || !strings.Contains(out, `"revision":1`) || !strings.Contains(out, `"committed":true`) {
		t.Fatal(out, stderr, err)
	}
	out, stderr, err = executeCommand(t, Dependencies{}, "profile", "show", "--json")
	if err != nil || stderr != "" || !strings.Contains(out, `hello\nworld`) || !strings.Contains(out, `"labels":{}`) || !strings.Contains(out, `"warnings":[]`) {
		t.Fatal(out, stderr, err)
	}
	entries, _ := os.ReadDir(state)
	if len(entries) != 1 || entries[0].Name() != "identity.key" {
		t.Fatalf("unexpected state: %v", entries)
	}
}
