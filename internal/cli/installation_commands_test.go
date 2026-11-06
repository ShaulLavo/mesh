package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestUpdateCommandCopiesSurviveRemoteInspection(t *testing.T) {
	host := update.Host{ID: "remote", MachineName: "mac"}
	copies := []updateinstall.CommandCopy{{Path: "/opt/homebrew/bin/mesh", Target: "/opt/homebrew/Caskroom/mesh/0.1.38/mesh", Owner: "homebrew-cask"}}
	caller := updateCallFunc(func(_ context.Context, _ update.Host, _ string, _, output any) error {
		*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID}, Commands: copies}
		return nil
	})
	targets, _ := inspectUpdateTargets(t.Context(), caller, []update.Host{host})
	if len(targets) != 1 || len(targets[0].Commands) != 1 || targets[0].Commands[0] != copies[0] {
		t.Fatalf("remote command installations lost: %+v", targets)
	}
}

func TestUpdateCommandCopiesAppearInEveryOutput(t *testing.T) {
	command := updateinstall.CommandCopy{Path: "/opt/homebrew/bin/mesh", Owner: "homebrew-cask"}
	target := update.Target{Host: update.Host{MachineName: "mac"}, State: update.Updated, Commands: []updateinstall.CommandCopy{command}}
	preview := updatePreview{Targets: []update.Target{target}, Release: release.Manifest{Version: "v0.1.255"}}
	run := update.Run{Targets: preview.Targets, Release: preview.Release}
	for _, details := range []bool{false, true} {
		checkCommandCopyOutput(t, func(out *bytes.Buffer) error { return printUpdatePreview(out, preview, false, details) }, command.Path)
		checkCommandCopyOutput(t, func(out *bytes.Buffer) error { return printUpdateRunMode(out, run, false, details) }, command.Path)
	}
	checkCommandCopyJSON(t, func(out *bytes.Buffer) error { return printUpdatePreview(out, preview, true, false) }, command.Path)
	checkCommandCopyJSON(t, func(out *bytes.Buffer) error { return printUpdateRunMode(out, run, true, false) }, command.Path)
}

func checkCommandCopyOutput(t *testing.T, render func(*bytes.Buffer) error, path string) {
	t.Helper()
	var out bytes.Buffer
	if err := render(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{path, "Mesh updates do not maintain this copy", "brew upgrade --cask shaullavo/mesh/mesh"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output omitted %q: %s", want, &out)
		}
	}
}

func checkCommandCopyJSON(t *testing.T, render func(*bytes.Buffer) error, path string) {
	t.Helper()
	var out bytes.Buffer
	if err := render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"commands":[{"path":"`+path+`"`) {
		t.Fatalf("structured output lost command copies: %s", &out)
	}
}

func TestClientOnlyUpdateFindsSeparateCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "bin")
	t.Setenv("PATH", dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mesh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil { //nolint:gosec // executable fixture is never run by discovery
		t.Fatal(err)
	}
	app := &application{}
	local := update.Host{ID: "local"}
	preview := app.reviewLocalUpdateBuild(updateEnvironment{local: local}, updatePreview{ClientOnly: true, Targets: []update.Target{{Host: local}}})
	found := false
	for _, command := range preview.Targets[0].Commands {
		found = found || command.Path == path
	}
	if !found {
		t.Fatalf("client-only copies = %+v", preview.Targets[0].Commands)
	}
}

func TestInstallationCheckReportsHomebrewCommandWithoutExecutingIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	brew := filepath.Join(home, "Caskroom", "mesh", "0.1.38", "mesh")
	if err := os.MkdirAll(filepath.Dir(brew), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil { //nolint:gosec // executable fixture is never run by discovery
		t.Fatal(err)
	}
	dir := filepath.Join(home, "brew-bin")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mesh")
	if err := os.Symlink(brew, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	checkCommandCopyOutput(t, func(out *bytes.Buffer) error {
		return printInstallationCommands(out, filepath.Join(home, "managed", "mesh"))
	}, path)
}

func TestSavedClientOnlyUpdateFindsSeparateCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "bin")
	t.Setenv("PATH", dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mesh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil { //nolint:gosec // executable fixture must be discovered without execution
		t.Fatal(err)
	}
	status := updateinstall.Status{
		Phase:    updateinstall.Committed,
		Settings: updateinstall.Settings{StateDir: filepath.Join(home, "state"), Executable: filepath.Join(home, "managed", "mesh"), ClientOnly: true},
		Request:  updateinstall.Request{TargetID: "local", Manifest: release.Manifest{Version: "v0.1.255"}},
	}
	run := runFromInstallation(status)
	if len(run.Targets) != 1 || len(run.Targets[0].Commands) != 1 || run.Targets[0].Commands[0].Path != path {
		t.Fatalf("saved client-only update lost separate commands: %+v", run.Targets)
	}
	checkCommandCopyJSON(t, func(out *bytes.Buffer) error { return printUpdateRunMode(out, run, true, false) }, path)
}
