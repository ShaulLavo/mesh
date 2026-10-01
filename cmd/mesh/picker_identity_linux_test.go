//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/testenv"
)

const pickerCreateFixtureVariable = "MESH_TEST_PICKER_FIRST_CREATE"

func TestPickerFirstCreateEstablishesIdentityAtSpawn(t *testing.T) {
	if os.Getenv(pickerCreateFixtureVariable) == "1" {
		runPickerFirstCreate(t)
		return
	}
	root := t.TempDir()
	stateDir, err := os.MkdirTemp(os.TempDir(), "picker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(stateDir); err != nil {
			t.Error(err)
		}
	})
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "busctl"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // isolated executable fixture prevents calls to the real user manager
		t.Fatal(err)
	}
	shell := filepath.Join(bin, "fixture-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nsleep 0.2\nprintf 'first-create-complete\\n'\n"), 0o700); err != nil { //nolint:gosec // the private shell fixture must be executable
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestPickerFirstCreateEstablishesIdentityAtSpawn$") //nolint:gosec // isolated child of this test binary
	child.Env = append(testenv.ForProcess(root), pickerCreateFixtureVariable+"=1", "MESH_STATE_DIR="+stateDir,
		"MESH_CONFIG_DIR="+filepath.Join(root, "config"), "SHELL="+shell, "PATH="+bin+":"+os.Getenv("PATH"))
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("first-create subprocess: %v\n%s", err, output)
	}
}

func runPickerFirstCreate(t *testing.T) {
	t.Helper()
	t.Setenv(runMainVariable, "1")
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close() //nolint:errcheck // test resource cleanup
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close() //nolint:errcheck // test resource cleanup
	called := false
	command := cli.NewCommand(cli.Dependencies{Stdin: input, Stdout: output, Stderr: output,
		Containment: func(context.Context) []protocol.SessionIdentity { return nil },
		Picker: func(context.Context, cli.PickerInput) (cli.PickerSelection, error) {
			called = true
			if _, err := identity.Load(os.Getenv("MESH_STATE_DIR")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("picker created identity before selection: %v", err)
			}
			return cli.PickerSelection{HostAlias: "this host", New: true}, nil
		},
	})
	command.SetArgs([]string{"--raw"})
	if err := command.ExecuteContext(t.Context()); err != nil || !called {
		t.Fatalf("first picker create = %v, called = %t", err, called)
	}
	if _, err := identity.Load(os.Getenv("MESH_STATE_DIR")); err != nil {
		t.Fatalf("Spawn did not establish identity: %v", err)
	}
	contents, err := os.ReadFile(output.Name())
	if err != nil || !strings.Contains(string(contents), "first-create-complete") {
		t.Fatalf("new worker output = %q, %v", contents, err)
	}
}
