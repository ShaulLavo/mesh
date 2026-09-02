package worker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/shaul/mesh/internal/paths"
)

func TestLaunchDetachedRejectsInvalidConfigBeforeCreatingState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	tests := []struct {
		name string
		cfg  LaunchConfig
	}{
		{name: "missing sessions directory", cfg: LaunchConfig{Command: []string{"sh"}}},
		{name: "missing command", cfg: LaunchConfig{SessionsDir: root}},
		{name: "empty executable", cfg: LaunchConfig{SessionsDir: root, Command: []string{""}}},
		{name: "oversized terminal", cfg: LaunchConfig{SessionsDir: root, Command: []string{"sh"}, Cols: maxTerminalDimension + 1, Rows: 24}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LaunchDetached(tt.cfg); err == nil {
				t.Fatal("LaunchDetached accepted invalid configuration")
			}
		})
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("invalid launches created state at %s", root)
	}
}

func TestReserveSessionDirCreatesDistinctPrivateDirectories(t *testing.T) {
	root := t.TempDir()
	firstID, firstDir, err := reserveSessionDir(root)
	if err != nil {
		t.Fatal(err)
	}
	secondID, secondDir, err := reserveSessionDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID || firstDir == secondDir {
		t.Fatalf("reserved duplicate sessions %q and %q", firstID, secondID)
	}
	for _, dir := range []string{firstDir, secondDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Fatalf("session directory %s mode = %o, want 700", dir, mode)
		}
		marker, err := os.Stat(paths.Launching(dir))
		if err != nil {
			t.Fatal(err)
		}
		if mode := marker.Mode().Perm(); mode != 0o600 {
			t.Fatalf("launch marker %s mode = %o, want 600", marker.Name(), mode)
		}
	}
}

func TestLaunchDetachedCleansReservationWhenProcessCannotStart(t *testing.T) {
	root := t.TempDir()
	_, err := LaunchDetached(LaunchConfig{
		SessionsDir: root,
		Executable:  filepath.Join(t.TempDir(), "missing-mesh"),
		Command:     []string{"sh"},
	})
	if err == nil {
		t.Fatal("LaunchDetached with missing executable succeeded")
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed launch left state entries: %v", entries)
	}
}

func TestWithTermReplacesTheServiceEnvironment(t *testing.T) {
	t.Parallel()

	// The daemon is a service with no TERM, so a session inherited none and
	// shell startup that branches on the terminal type took the wrong path.
	got := withTerm([]string{"HOME=/home/x", "TERM=dumb", "PATH=/usr/bin"}, "xterm-256color")
	want := []string{"HOME=/home/x", "PATH=/usr/bin", "TERM=xterm-256color"}
	if !slices.Equal(got, want) {
		t.Fatalf("withTerm() = %q, want %q", got, want)
	}
	if only := withTerm([]string{"HOME=/home/x"}, "screen"); !slices.Equal(only, []string{"HOME=/home/x", "TERM=screen"}) {
		t.Fatalf("withTerm() on an env without TERM = %q", only)
	}
	// An empty term must not add a bare TERM= that overrides a real one later.
	if unchanged := withTerm([]string{"TERM=vt100"}, ""); !slices.Equal(unchanged, []string{"TERM=vt100"}) {
		t.Fatalf("withTerm() with no term = %q", unchanged)
	}
}
