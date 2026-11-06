package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
)

type noProfileInput struct{ t *testing.T }

func (r noProfileInput) Read([]byte) (int, error) {
	r.t.Error("stdin read without explicit --file -")
	return 0, io.ErrUnexpectedEOF
}

func runProfile(t *testing.T, input io.Reader, args ...string) (agentapi.Envelope, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	command := NewCommand(Dependencies{})
	command.SetIn(input)
	command.SetOut(&out)
	command.SetErr(&stderr)
	command.SetArgs(args)
	err := command.ExecuteContext(t.Context())
	var envelope agentapi.Envelope
	if decode := json.Unmarshal(out.Bytes(), &envelope); decode != nil {
		t.Fatalf("expected one JSON object, got %q: %v", out.String(), decode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("duplicate diagnostic: %s", stderr.String())
	}
	return envelope, out.String(), err
}

func TestProfileEarlyErrorsDoNotReadState(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	t.Setenv("MESH_CONFIG_DIR", config)
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("EDITOR", "exit 88")
	cases := []struct {
		args      []string
		operation string
	}{
		{[]string{"profile", "nope", "--json"}, "profile.show"},
		{[]string{"profile", "--json"}, "profile.show"},
		{[]string{"profile", "show", "--json", "extra"}, "profile.show"},
		{[]string{"profile", "show", "--json", "--bogus"}, "profile.show"},
		{[]string{"profile", "set", "--json"}, "profile.set"},
		{[]string{"profile", "edit", "--json"}, "profile.edit"},
		{[]string{"profile", "edit", "--json", "--bogus"}, "profile.edit"},
		{[]string{"profile", "set", "--json", "--file", "--json=false", "--expected-revision", "bad"}, "profile.set"},
		{[]string{"profile", "show", "--json", "--", "--json=false"}, "profile.show"},
		{[]string{"profile", "nope", "--json", "--file", "set"}, "profile.show"},
	}
	for _, tc := range cases {
		result, output, err := runProfile(t, noProfileInput{t}, tc.args...)
		if result.Operation != tc.operation || result.Status != agentapi.StatusError || result.Data != nil || len(result.Errors) != 1 || result.Errors[0].Code != agentapi.UsageInvalid {
			t.Fatalf("%v: %s", tc.args, output)
		}
		if code, ok := StatusCode(err); !ok || code != 2 {
			t.Fatalf("%v: %v", tc.args, err)
		}
	}
	for _, dir := range []string{state, config} {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("usage created %s: %v", dir, err)
		}
	}
}

func TestProfileExplicitStdinAndInvalidDocuments(t *testing.T) {
	state := t.TempDir()
	config := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	for _, document := range []string{`{"schemaVersion":1,"description":"\u202e"}`, strings.Repeat(" ", 32769), `{"schemaVersion":1,"unknown":1}`} {
		result, output, err := runProfile(t, strings.NewReader(document), "profile", "set", "--file", "-", "--expected-revision", "0", "--json")
		if result.Errors[0].Code != agentapi.ProfileInvalid {
			t.Fatal(output, err)
		}
		if code, ok := StatusCode(err); !ok || code != 2 {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(config)
	if len(entries) != 0 {
		t.Fatal("invalid input created profile/lock")
	}
	result, output, err := runProfile(t, strings.NewReader(`{"schemaVersion":1}`), "profile", "set", "--file", "-", "--expected-revision", "0", "--json")
	if err != nil || result.Operation != "profile.set" || !strings.Contains(output, `"revision":1`) {
		t.Fatal(output, err)
	}
	result, output, err = runProfile(t, strings.NewReader(`{"schemaVersion":1,"description":"different"}`), "profile", "set", "--file", "-", "--expected-revision", "0", "--json")
	if result.Errors[0].Code != agentapi.ProfileConflict {
		t.Fatal(output, err)
	}
}

func TestProfileEditFixtureEditor(t *testing.T) {
	state := t.TempDir()
	config := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	editor := filepath.Join(t.TempDir(), "editor")
	// #nosec G306 -- Private executable fixture editor, not user configuration.
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s' '{\"schemaVersion\":1,\"instructions\":\"edited\"}' > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", editor)
	t.Setenv("EDITOR", "exit 89")
	output, stderr, err := executeCommand(t, Dependencies{}, "profile", "edit")
	if err != nil || stderr != "" || output != "Profile saved.\n" {
		t.Fatal(output, stderr, err)
	}
	store := hostprofile.OpenHostStore(config, host.ID)
	snapshot, err := store.Read()
	if err != nil || snapshot.Value.Instructions != "edited" {
		t.Fatal(snapshot, err)
	}
	entries, _ := os.ReadDir(state)
	if len(entries) != 1 {
		t.Fatalf("successful edit retained temporary files: %v", entries)
	}
	// #nosec G306 -- Private executable fixture editor, not user configuration.
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s' '{' > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, _, err = executeCommand(t, Dependencies{}, "profile", "edit")
	if err == nil || !strings.Contains(err.Error(), "editable copy kept at") {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(state)
	if len(entries) != 2 {
		t.Fatal(entries)
	}
	after, err := store.Read()
	if err != nil || *after.Revision != 1 {
		t.Fatal(after, err)
	}
}

func TestProfileHumanAndParsedBooleanSemantics(t *testing.T) {
	state := t.TempDir()
	config := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = hostprofile.OpenHostStore(config, host.ID).Set(context.Background(), 0, hostprofile.Editable{SchemaVersion: 1, Description: "description", Labels: map[string]string{"role": "test"}, Warnings: []string{"warning"}, Instructions: "one\ntwo"})
	if err != nil {
		t.Fatal(err)
	}
	out, stderr, err := executeCommand(t, Dependencies{}, "profile", "show", "--json", "--json=false")
	if err != nil || stderr != "" || strings.Contains(out, `"schemaVersion"`) || !strings.Contains(out, "Revision: 1") || !strings.Contains(out, "one\ntwo") {
		t.Fatal(out, stderr, err)
	}
	result, output, err := runProfile(t, noProfileInput{t}, "profile", "show", "--json=false", "--json=true", "--privacy", "--privacy=false")
	if err != nil || result.Status != agentapi.StatusOK {
		t.Fatal(output, err)
	}
	result, output, err = runProfile(t, noProfileInput{t}, "profile", "show", "--json", "--privacy")
	if result.Errors[0].Code != agentapi.UsageInvalid {
		t.Fatal(output, err)
	}
}
