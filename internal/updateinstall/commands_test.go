package updateinstall

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOtherCommandsRetainsCopiesThatCanDrift(t *testing.T) {
	root := t.TempDir()
	managed := commandFixture(t, filepath.Join(root, "managed", "mesh"))
	brew := commandFixture(t, filepath.Join(root, "Caskroom", "mesh", "0.1.38", "mesh"))
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(brew, filepath.Join(bin, "mesh")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Dir(managed), alias); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(root, "hardlink")
	if err := os.MkdirAll(hardlink, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(managed, filepath.Join(hardlink, "mesh")); err != nil {
		t.Fatal(err)
	}
	copies := OtherCommands(managed, []string{bin, bin, alias, hardlink, "", "."})
	if len(copies) != 2 || copies[0].Path != filepath.Join(bin, "mesh") || copies[0].Owner != "homebrew-cask" {
		t.Fatalf("separate installations = %+v", copies)
	}
	if !strings.Contains(copies[0].Remedy(), "brew upgrade --cask shaullavo/mesh/mesh") {
		t.Fatalf("unqualified cask remedy: %s", copies[0].Remedy())
	}
	replacement := commandFixture(t, filepath.Join(root, "replacement"))
	if err := os.Rename(replacement, managed); err != nil {
		t.Fatal(err)
	}
	if got := OtherCommands(managed, []string{bin, alias, hardlink}); len(got) != 2 {
		t.Fatalf("atomic update hid other commands: %+v", got)
	}
}

func TestOtherCommandsSkipsNonCommands(t *testing.T) {
	root := t.TempDir()
	managed := commandFixture(t, filepath.Join(root, "managed", "mesh"))
	nonExecutable := commandFixture(t, filepath.Join(root, "non-executable", "mesh"))
	if err := os.Chmod(nonExecutable, 0600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.MkdirAll(filepath.Join(directory, "mesh"), 0700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := os.MkdirAll(fifo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(fifo, "mesh"), 0700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(broken, "mesh")); err != nil {
		t.Fatal(err)
	}
	if got := OtherCommands(managed, []string{filepath.Dir(nonExecutable), directory, fifo, broken, "relative", ""}); len(got) != 0 {
		t.Fatalf("non-commands reported: %+v", got)
	}
}

func TestHomebrewInstallationErrorNamesTheOwningCask(t *testing.T) {
	root := t.TempDir()
	brew := commandFixture(t, filepath.Join(root, "Caskroom", "mesh", "0.1.38", "mesh"))
	link := filepath.Join(root, "mesh")
	if err := os.Symlink(brew, link); err != nil {
		t.Fatal(err)
	}
	err := ValidateInstallationPath(link)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade --cask shaullavo/mesh/mesh") {
		t.Fatalf("Homebrew repair was not actionable: %v", err)
	}
}

func TestCommandPathPrefersManagedInstallation(t *testing.T) {
	managed := "/custom/mesh/bin/mesh"
	path := CommandPath(managed, "/opt/homebrew/bin:/usr/bin:/custom/mesh/bin")
	if path != "/custom/mesh/bin:/opt/homebrew/bin:/usr/bin" {
		t.Fatalf("daemon PATH = %q", path)
	}
}

func commandFixture(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil { //nolint:gosec // discovery requires an executable fixture but must not run it
		t.Fatal(err)
	}
	return path
}
