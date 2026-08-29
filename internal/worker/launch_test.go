package worker

import (
	"os"
	"path/filepath"
	"testing"
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
	}
}
