package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestAgentTerminalUsageExit(t *testing.T) {
	if os.Getenv("MESH_TEST_TERMINAL_USAGE") == "1" {
		os.Args = []string{"mesh", "--host", "pc", "tool"}
		main()
		return
	}
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestAgentTerminalUsageExit$") //nolint:gosec // isolated current test executable
	child.Env = append(testenv.ForProcess(root), "MESH_TEST_TERMINAL_USAGE=1", "MESH_CONFIG_DIR="+filepath.Join(root, "config"), "MESH_STATE_DIR="+filepath.Join(root, "state"))
	output, err := child.Output()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 2 {
		t.Fatalf("terminal usage should exit 2: %v", err)
	}
	if len(output) != 0 || !strings.Contains(string(exited.Stderr), "put the command after --") {
		t.Fatalf("human error missing: %q %q", output, exited.Stderr)
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

func TestAgentJSONFlagOperands(t *testing.T) {
	if arguments := os.Getenv("MESH_TEST_AGENT_ARGUMENTS"); arguments != "" {
		var args []string
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"mesh"}, args...)
		main()
		os.Exit(0)
	}
	for _, test := range []struct {
		args []string
		json bool
	}{
		{[]string{"serve", "ls", "--json", "--leave-key", "--json=false", "--bogus"}, true},
		{[]string{"serve", "ls", "--leave-key", "--json", "--bogus"}, false},
		{[]string{"serve", "ls", "--json=invalid"}, true},
		{[]string{"serve", "ls", "--json=invalid", "--json=false"}, true},
		{[]string{"--leave-key", "--", "serve", "ls", "--json", "--bogus"}, true},
		{[]string{"serve", "ls", "--json=false", "--json", "--bogus"}, true},
		{[]string{"serve", "ls", "--json", "--json=false", "--bogus"}, false},
	} {
		checkAgentJSONFlagOperands(t, test.args, test.json)
	}
}

func checkAgentJSONFlagOperands(t *testing.T, args []string, wantJSON bool) {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestAgentJSONFlagOperands$") //nolint:gosec // isolated current test executable
	child.Env = append(testenv.ForProcess(root), "MESH_TEST_AGENT_ARGUMENTS="+string(arguments), "MESH_CONFIG_DIR="+filepath.Join(root, "config"), "MESH_STATE_DIR="+filepath.Join(root, "state"))
	stdout, err := child.Output()
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatalf("%v: expected usage failure: %v", args, err)
	}
	if !wantJSON {
		if len(stdout) != 0 || len(exited.Stderr) == 0 {
			t.Fatalf("%v: expected human diagnostic: %q %q", args, stdout, exited.Stderr)
		}
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(stdout)))
	var result struct {
		Operation string
		Status    string
		Errors    []struct{ Code string }
	}
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("%v: no usage envelope: %q: %v", args, stdout, err)
	}
	if exited.ExitCode() != 2 || result.Operation != "services.list" || result.Status != "error" || len(result.Errors) != 1 || result.Errors[0].Code != "usage.invalid" {
		t.Fatalf("%v: wrong usage result, exit %d: %s", args, exited.ExitCode(), stdout)
	}
	if err := decoder.Decode(&result); err != io.EOF || len(exited.Stderr) != 0 {
		t.Fatalf("%v: extra output: %v, %q", args, err, exited.Stderr)
	}
}
