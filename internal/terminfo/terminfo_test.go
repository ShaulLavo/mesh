package terminfo

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransferTerminalDefinition(t *testing.T) {
	if _, err := exec.LookPath("tic"); err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("infocmp"); err != nil {
		t.Skip(err)
	}
	source := t.TempDir()
	command := exec.Command("tic", "-x", "-o", source, "-") //nolint:gosec // fixed tool compiles into the isolated fixture directory
	command.Stdin = bytes.NewBufferString("mesh-fixture|Mesh fixture terminal,\n am, cols#80, lines#24, clear=\\E[H\\E[2J, setrgbf=\\E[38:2:%p1%d:%p2%d:%p3%dm,\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("tic: %s: %v", output, err)
	}
	t.Setenv("TERMINFO", source)
	data := Capture(context.Background(), "mesh-fixture")
	if len(data) == 0 {
		t.Fatal("did not capture definition")
	}
	target := t.TempDir()
	t.Setenv("HOME", target)
	t.Setenv("TERMINFO", filepath.Join(target, ".terminfo"))
	t.Setenv("TERMINFO_DIRS", filepath.Join(target, ".terminfo"))
	if err := exec.Command("infocmp", "mesh-fixture").Run(); err == nil {
		t.Fatal("fixture was already available on target")
	}
	if err := Install(context.Background(), "mesh-fixture", data); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), "mesh-fixture", data); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("infocmp", "-x", "mesh-fixture")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte("setrgbf=")) {
		t.Fatalf("lost extended capability: %s", output)
	}
	command = exec.Command("clear")
	command.Env = append(os.Environ(), "TERM=mesh-fixture")
	if output, err := command.Output(); err != nil || !bytes.Contains(output, []byte("\x1b[H\x1b[2J")) {
		t.Fatalf("clear=%q, %v", output, err)
	}
}

func TestRejectUnsafeDefinitions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"../bad", "/bad", "-bad", "bad\x00", ""} {
		if err := Install(context.Background(), name, []byte("bad")); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if err := Install(context.Background(), "safe", make([]byte, MaximumBytes+1)); err == nil {
		t.Fatal("accepted oversized definition")
	}
	if err := Install(context.Background(), "safe", []byte("bad")); err == nil {
		t.Fatal("accepted malformed definition")
	}
}

func TestMissingLocalToolsDoNotChangeTerminal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := Capture(t.Context(), "xterm-ghostty"); len(got) != 0 {
		t.Fatalf("capture without tools = %q", got)
	}
}

func TestMissingRemoteCompilerNamesTheRequiredAction(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Install(t.Context(), "mesh-fixture", []byte("mesh-fixture|Mesh fixture terminal, clear=\\E[H\\E[2J,\n"))
	if err == nil || !strings.Contains(err.Error(), "install ncurses terminal tools on this host") {
		t.Fatalf("compiler error = %v", err)
	}
}
