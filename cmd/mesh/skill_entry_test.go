package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/skills"
	"github.com/shaul/mesh/internal/testenv"
)

func TestSkillStandaloneEntry(t *testing.T) {
	if encoded := os.Getenv("MESH_TEST_SKILL_ARGS"); encoded != "" {
		var args []string
		if err := json.Unmarshal([]byte(encoded), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"mesh"}, args...)
		main()
		os.Exit(0)
	}
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{true: "malformed", false: "absent"}[malformed], func(t *testing.T) { checkSkillStandalone(t, malformed) })
	}
}
func checkSkillStandalone(t *testing.T, malformed bool) {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	if malformed {
		mustSkill(t, os.Mkdir(config, 0700))
		mustSkill(t, os.Mkdir(state, 0700))
		for _, name := range []string{"hosts.json", "domains.json", "skills.json"} {
			mustSkill(t, os.WriteFile(filepath.Join(config, name), []byte("{"), 0600))
		}
		mustSkill(t, os.WriteFile(filepath.Join(state, "key"), []byte("malformed"), 0600))
	}
	before := snapshotSkillEntry(t, root)
	for _, file := range []string{"", "references/commands.md", "references/safety.md"} {
		checkSkillEntryFile(t, root, config, state, file)
	}
	after := snapshotSkillEntry(t, root)
	if !bytes.Equal(before, after) {
		t.Fatalf("standalone show touched fixture paths before=%s after=%s", before, after)
	}
	output, stderr, exit := runSkillEntry(t, root, config, state, []string{"skill", "show", "--json"})
	var result struct {
		Operation, Status string
		Errors            []struct{ Code string }
	}
	mustSkill(t, json.Unmarshal(output, &result))
	if exit != 2 || result.Operation != "skill.show" || len(result.Errors) != 1 || result.Errors[0].Code != "usage.invalid" || len(stderr) != 0 {
		t.Fatalf("unsupported JSON mixed output: %d %s %s", exit, output, stderr)
	}
	output, stderr, exit = runSkillEntry(t, root, config, state, []string{"skill", "show", "--", "--json"})
	if exit != 2 || len(output) != 0 || len(stderr) == 0 {
		t.Fatalf("separator enabled JSON: %d %s %s", exit, output, stderr)
	}
}
func checkSkillEntryFile(t *testing.T, root, config, state, file string) {
	t.Helper()
	args := []string{"skill", "show"}
	if file != "" {
		args = append(args, "--file", file)
	}
	output, stderr, exit := runSkillEntry(t, root, config, state, args)
	want, err := skills.Show(file)
	mustSkill(t, err)
	if exit != 0 || !bytes.Equal(output, want) || len(stderr) != 0 {
		t.Fatalf("exact show %q: %d %s %s", file, exit, output, stderr)
	}
}
func runSkillEntry(t *testing.T, root, config, state string, args []string) ([]byte, []byte, int) {
	t.Helper()
	binary, err := os.Executable()
	mustSkill(t, err)
	encoded, err := json.Marshal(args)
	mustSkill(t, err)
	command := &exec.Cmd{Path: binary, Args: []string{binary, "-test.run=^TestSkillStandaloneEntry$"}}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	command.Env = append(testenv.ForProcess(root), "MESH_TEST_SKILL_ARGS="+string(encoded), "MESH_CONFIG_DIR="+config, "MESH_STATE_DIR="+state)
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
func mustSkill(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func snapshotSkillEntry(t *testing.T, root string) []byte {
	t.Helper()
	dir, err := os.OpenRoot(root)
	mustSkill(t, err)
	defer func() { _ = dir.Close() }()
	var snapshot bytes.Buffer
	mustSkill(t, fs.WalkDir(dir.FS(), ".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		snapshot.WriteString(path)
		if !entry.IsDir() {
			data, err := dir.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read fixture snapshot: %w", err)
			}
			snapshot.Write(data)
		}
		return nil
	}))
	return snapshot.Bytes()
}

func TestSkillExemptionLeavesOrdinaryInitialization(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	mustSkill(t, os.Mkdir(config, 0700))
	mustSkill(t, os.WriteFile(filepath.Join(config, "domains.json"), []byte("{"), 0600))
	for _, args := range [][]string{{"hosts", "--json"}, {"skill", "status", "--json"}, {"shell-init", "bash"}, {"--host", "HOST_ID", "--", "tool", "--json"}} {
		output, stderr, exit := runSkillEntry(t, root, config, state, args)
		if exit == 0 {
			t.Fatalf("ordinary initialization bypassed: %v", args)
		}
		if args[0] == "hosts" || args[0] == "skill" {
			var envelope map[string]any
			mustSkill(t, json.Unmarshal(output, &envelope))
			if len(stderr) != 0 {
				t.Fatalf("repeated JSON diagnostic %s", stderr)
			}
			continue
		}
		if len(output) != 0 || len(stderr) == 0 {
			t.Fatalf("ordinary error changed: %v %s %s", args, output, stderr)
		}
	}
}
