package sshd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRequestRejectsInvalidLaunchFields(t *testing.T) {
	for _, request := range []RunRequest{
		{Cwd: "a\x00b"}, {Env: []string{"MISSING_EQUALS"}},
		{Env: []string{"=empty-name"}}, {Env: []string{"KEY=\x00"}},
	} {
		payload, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseRunRequest(payload); err == nil {
			t.Fatalf("accepted %+v", request)
		}
	}
}

func TestRunDirectoryAndEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MESH_RUN_INHERITED", "kept")
	t.Setenv("MESH_RUN_OVERRIDE", "original")
	for _, cwd := range []string{"", "relative", filepath.Join(home, "absolute")} {
		assertRunOptions(t, home, cwd)
	}
	if os.Getenv("MESH_RUN_OVERRIDE") != "original" {
		t.Fatal("launch changed the daemon environment")
	}
}

func assertRunOptions(t *testing.T, home, cwd string) {
	t.Helper()
	want := cwd
	if !filepath.IsAbs(want) {
		want = filepath.Join(home, want)
	}
	if err := os.MkdirAll(want, 0700); err != nil {
		t.Fatal(err)
	}
	request := RunRequest{Args: []string{"/bin/sh", "-c", "printf '%s|%s|%s' \"$PWD\" \"$MESH_RUN_INHERITED\" \"$MESH_RUN_OVERRIDE\""},
		Cwd: cwd, Env: []string{"MESH_RUN_OVERRIDE=first", "MESH_RUN_OVERRIDE=last=with-equals"}}
	command, err := prepareProcess(t.Context(), nil, request)
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.Output()
	if err != nil || string(output) != want+"|kept|last=with-equals" {
		t.Fatalf("launch: %q, %v", output, err)
	}
	_, err = prepareProcess(t.Context(), nil, RunRequest{Args: []string{"/bin/true"}, Cwd: filepath.Join(home, "absent")})
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("unavailable directory: %v", err)
	}
}
