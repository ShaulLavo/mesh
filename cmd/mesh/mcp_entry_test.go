package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/testenv"
)

func TestMCPEntryStdoutCleanliness(t *testing.T) {
	if encoded := os.Getenv("MESH_TEST_MCP_ARGS"); encoded != "" {
		var args []string
		if err := json.Unmarshal([]byte(encoded), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"mesh"}, args...)
		main()
		os.Exit(0)
	}

	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")

	// Usage and startup errors exit 2 with empty stdout and diagnostic on stderr.
	for _, args := range [][]string{
		{"mcp", "--bogus"},
		{"mcp", "unexpected-arg"},
		{"mcp", "--privacy"},
		{"--privacy", "mcp"},
	} {
		stdout, stderr, exit := runMCPEntry(t, root, config, state, args, "")
		if exit != 2 || len(stdout) != 0 || len(stderr) == 0 {
			t.Fatalf("%v: exit=%d stdout=%q stderr=%q (want exit=2, empty stdout, non-empty stderr)", args, exit, stdout, stderr)
		}
	}

	// Clean stdio session in an uninitialized offline directory: mesh_skill and
	// mesh_hosts_list emit only valid JSON-RPC lines on stdout and exit 0 on EOF.
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"mesh_skill","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"mesh_hosts_list","arguments":{"cached":true}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"legacy","version":"1"}}}`,
	}
	lines, stderr, exit := runMCPInteractive(t, root, config, state, requests)
	if exit != 0 {
		t.Fatalf("mesh mcp exit=%d stderr=%q lines=%q", exit, stderr, lines)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 JSON-RPC lines on stdout, got %d: %q", len(lines), lines)
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil || obj["jsonrpc"] != "2.0" {
			t.Fatalf("line %d is not JSON-RPC 2.0: %q", i, line)
		}
	}
}

func runMCPInteractive(t *testing.T, root, config, state string, requests []string) ([]string, []byte, int) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal([]string{"mcp"})
	if err != nil {
		t.Fatal(err)
	}
	command := &exec.Cmd{Path: binary, Args: []string{binary, "-test.run=^TestMCPEntryStdoutCleanliness$"}}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	command.Env = append(testenv.ForProcess(root), "MESH_TEST_MCP_ARGS="+string(encoded), "MESH_CONFIG_DIR="+config, "MESH_STATE_DIR="+state)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	lines := make([]string, 0, len(requests))
	for _, req := range requests {
		if _, err := stdin.Write([]byte(req + "\n")); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read MCP reply: %v (stderr=%q)", err, stderr.String())
		}
		lines = append(lines, strings.TrimSuffix(line, "\n"))
	}
	_ = stdin.Close()
	err = command.Wait()
	if err == nil {
		return lines, stderr.Bytes(), 0
	}
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatal(err)
	}
	return lines, stderr.Bytes(), exited.ExitCode()
}

func runMCPEntry(t *testing.T, root, config, state string, args []string, stdin string) ([]byte, []byte, int) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	command := &exec.Cmd{Path: binary, Args: []string{binary, "-test.run=^TestMCPEntryStdoutCleanliness$"}}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	command.Env = append(testenv.ForProcess(root), "MESH_TEST_MCP_ARGS="+string(encoded), "MESH_CONFIG_DIR="+config, "MESH_STATE_DIR="+state)
	command.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err == nil {
		return output, stderr.Bytes(), 0
	}
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatal(err)
	}
	return output, stderr.Bytes(), exited.ExitCode()
}
