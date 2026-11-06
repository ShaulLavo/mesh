package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/testenv"
)

func TestAgentEarlyJSONFailures(t *testing.T) {
	if os.Getenv("MESH_TEST_AGENT_ENTRY") == "1" {
		os.Args = []string{"mesh", "serve", "list", "--json"}
		main()
		return
	}
	for _, file := range []string{"domains.json", "hosts.json"} {
		t.Run(file, func(t *testing.T) { checkEarlyJSONFailure(t, file) })
	}
}

func checkEarlyJSONFailure(t *testing.T, file string) {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "missing-state")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, file), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestAgentEarlyJSONFailures$") //nolint:gosec // isolated current test executable
	child.Env = append(testenv.ForProcess(root), "MESH_TEST_AGENT_ENTRY=1", "MESH_CONFIG_DIR="+config, "MESH_STATE_DIR="+state)
	stdout, err := child.Output()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 1 {
		t.Fatalf("expected exit 1: %v", err)
	}
	var result struct {
		Operation string
		Status    string
		Errors    []struct{ Code string }
	}
	if err := json.Unmarshal(stdout, &result); err != nil {
		t.Fatalf("early failure has no envelope: %q: %v", stdout, err)
	}
	if result.Operation != "services.list" || result.Status != "error" || len(result.Errors) != 1 || result.Errors[0].Code != "state.unavailable" {
		t.Fatalf("wrong error: %s", stdout)
	}
	if len(exited.Stderr) != 0 {
		t.Fatalf("repeated diagnostic: %q", exited.Stderr)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatalf("early read created state: %v", err)
	}
}
