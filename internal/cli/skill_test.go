package cli

import (
	"encoding/json"
	"errors"
	"github.com/shaul/mesh/internal/skills"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
)

func TestSkillStatusRegisteredCommand(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "absent-config"))
	t.Setenv("MESH_STATE_DIR", filepath.Join(root, "absent-state"))
	stdout, stderr, err := executeCommand(t, Dependencies{}, "skill", "status", "--dir", root, "--json")
	if err != nil {
		t.Fatalf("status: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
	var envelope struct {
		Operation string
		Data      agentapi.SkillResult
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Operation != "skill.status" || len(envelope.Data.Roots) != 1 || envelope.Data.Roots[0].State != "not-installed" {
		t.Fatalf("unexpected result %s", stdout)
	}
}

func TestSkillShowSkipsOnlyOwnClientActivation(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	t.Setenv("MESH_CONFIG_DIR", config)
	t.Setenv("MESH_STATE_DIR", state)
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "hosts.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeCommand(t, Dependencies{}, "skill", "show")
	want, showErr := skills.Show("")
	if showErr != nil {
		t.Fatal(showErr)
	}
	if err != nil || stdout != string(want) || stderr != "" {
		t.Fatalf("show: %v %s %s", err, stdout, stderr)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "shell-init", "bash"); err == nil {
		t.Fatal("ordinary client activation was bypassed")
	}
	stdout, stderr, err = executeCommand(t, Dependencies{}, "skill", "status", "--dir", root, "--json")
	assertSkillFailure(t, stdout, stderr, err, "state.unavailable", 1)
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("show or status initialized identity state")
	}
}

func TestSkillJSONUsageRouting(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("MESH_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, args := range [][]string{{"skill", "--json"}, {"skill", "unknown", "--json"}, {"skill", "show", "--json"}, {"skill", "install", "--json"}, {"skill", "status", "--json", "--bogus"}} {
		stdout, stderr, err := executeCommand(t, Dependencies{}, args...)
		assertSkillFailure(t, stdout, stderr, err, "usage.invalid", 2)
	}
}

func assertSkillFailure(t *testing.T, stdout, stderr string, err error, code string, exit int) {
	t.Helper()
	var result struct {
		Status string
		Errors []struct{ Code string }
	}
	if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if result.Status != "error" || len(result.Errors) != 1 || result.Errors[0].Code != code {
		t.Fatalf("unexpected skill envelope %s", stdout)
	}
	if status, ok := StatusCode(err); !ok || status != exit || stderr != "" {
		t.Fatalf("exit=%v stderr=%s", err, stderr)
	}
}
