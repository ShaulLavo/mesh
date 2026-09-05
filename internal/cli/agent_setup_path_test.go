package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAgentSettingsUpdatePreservesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "tracked-settings.json")
	path := filepath.Join(root, "settings.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked-settings.json", path); err != nil {
		t.Fatal(err)
	}
	want := "{\"hooks\":{}}\n"
	if err := writeAgentSettings(path, []byte(want)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("install replaced the provider settings symlink with an independent file")
	}
	got, err := os.ReadFile(target) //nolint:gosec // target is the settings fixture inside t.TempDir
	if err != nil || string(got) != want {
		t.Fatalf("tracked provider settings = %q, %v; expected update through the symlink", got, err)
	}
}

func TestAgentSettingsUpdateRejectsDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "settings.json")
	if err := os.Symlink("missing-settings.json", path); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentSettings(path, []byte("{}\n")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling link update = %v, want missing target error", err)
	}
	if target, err := os.Readlink(path); err != nil || target != "missing-settings.json" {
		t.Fatalf("dangling link was changed: %q, %v", target, err)
	}
}

func TestAgentSettingsUpdateCreatesMissingRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider", "settings.json")
	if err := writeAgentSettings(path, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the settings fixture inside t.TempDir
	if err != nil || string(data) != "{}\n" {
		t.Fatalf("new settings = %q, %v", data, err)
	}
}
