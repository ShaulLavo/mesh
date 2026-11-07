package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionNoticeUsesExistingPlainEntryExemption(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	t.Setenv("MESH_CONFIG_DIR", config)
	t.Setenv("MESH_STATE_DIR", state)
	for _, args := range [][]string{{"version", "--licenses"}, {"version", "--licenses=false"}, {"version", "--licenses", "--json"}, {"version", "--json", "--licenses"}, {"version", "--licenses", "operand"}} {
		if !plainAgentCommand(args) || namingCommand(args) {
			t.Fatalf("version entry initializes deployment: %v", args)
		}
		if err := initializeDeployment(args); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{config, state} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("version entry created %s: %v", path, err)
		}
	}
}
