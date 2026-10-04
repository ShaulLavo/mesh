package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransitionFixtureRollbackSavedRecovery(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("release transition fixture requires bash")
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "scripts", "prove-release-transition.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const diagnostic = "retained daemon lost saved recovery session after candidate writes"
	var assertion string
	for line := range strings.SplitSeq(string(fixture), "\n") {
		if !strings.Contains(line, diagnostic) {
			continue
		}
		if assertion != "" {
			t.Fatal("rollback saved-recovery assertion must be unique")
		}
		assertion = line
	}
	if assertion == "" {
		t.Fatal("rollback saved-recovery assertion is missing")
	}
	root := t.TempDir()
	old := filepath.Join(root, "retained-cli")
	candidate := filepath.Join(root, "candidate-cli")
	// The restored daemon accepts its retained CLI; the candidate CLI rejects its older declaration.
	if err := os.WriteFile(old, []byte("#!/bin/sh\n[ \"$*\" = 'ls --daemon --all' ] || exit 2\n[ \"$MESH_FIXTURE_SAVED\" = present ] || exit 0\nprintf '0000\\n'\n"), 0o700); err != nil { //nolint:gosec // owner-only executable CLI fixture beneath t.TempDir
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { //nolint:gosec // owner-only executable CLI fixture beneath t.TempDir
		t.Fatal(err)
	}
	for _, state := range []string{"present", "missing"} {
		t.Run(state, func(t *testing.T) {
			setup := "set -euo pipefail\nold_binary=$1\ncandidate_binary=$2\nsaved_session=0000\nmesh() { \"$@\"; }\nfail() { printf '%s\\n' \"$*\" >&2; exit 42; }\n"
			command := exec.CommandContext(t.Context(), bash, "-c", setup+assertion, "fixture", old, candidate) //nolint:gosec // executes the checked-in assertion against private CLI fixtures
			command.Env = append(os.Environ(), "MESH_FIXTURE_SAVED="+state)
			output, err := command.CombinedOutput()
			if state == "present" && err != nil {
				t.Fatalf("restored CLI could not verify saved recovery: %v\n%s", err, output)
			}
			if state == "present" {
				return
			}
			if err == nil || !strings.Contains(string(output), diagnostic) {
				t.Fatalf("missing saved recovery must fail its assertion: error=%v output=%s", err, output)
			}
		})
	}
}
