package updatebootstrap

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusShellReportsMissingJournalEvenWithInstalledHelper(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "update", "helper")
	if err := os.MkdirAll(helper, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helper, "current"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil { //nolint:gosec // isolated executable that must never run without a receipt
		t.Fatal(err)
	}
	output, err := runStatusShell(t, root)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(string(output), "MESH_UPDATE_RECEIPT_UNAVAILABLE") {
		t.Fatalf("missing receipt = %q %v", output, err)
	}
}

func TestStatusShellReadsReceiptWithoutInstalledHelper(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	receipt := []byte("{\"schema\":1}\n")
	if err := os.WriteFile(filepath.Join(root, "update", "installation.json"), receipt, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := runStatusShell(t, root)
	if err != nil || string(output) != "MESH_UPDATE_RECEIPT="+string(receipt) {
		t.Fatalf("fallback receipt = %q %v", output, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "update"))
	if err != nil || len(entries) != 1 {
		t.Fatal("status command mutated installation state")
	}
}

func runStatusShell(t *testing.T, stateDir string) ([]byte, error) {
	t.Helper()
	args, err := StatusCommand(testRequest())
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(args[0], args[1:]...) //nolint:gosec // fixed read-only status script against isolated state
	command.Env = append(os.Environ(), "MESH_STATE_DIR="+stateDir)
	return command.CombinedOutput()
}
