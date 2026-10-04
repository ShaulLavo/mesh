package updateinstall

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestClientOnlyProbeUsesInstallationStateDirectory(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("client-only process probe needs a POSIX shell")
	}
	root := t.TempDir()
	state := filepath.Join(root, "configured-state")
	t.Setenv("MESH_STATE_DIR", filepath.Join(root, "other-state"))
	executable := filepath.Join(root, "client")
	contents := "#!/bin/sh\nprintf '{\"commit\":\"%s\"}\\n' \"$MESH_STATE_DIR\"\n"
	if err := os.WriteFile(executable, []byte(contents), 0o700); err != nil { //nolint:gosec // owned executable fixture must run the real process probe

		t.Fatal(err)
	}
	engine := Engine{cfg: Config{ClientOnly: true, Executable: executable, StateDir: state}}
	health, err := engine.probe(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if health.HostID != "owner" || health.Build.Commit != state {
		t.Fatal("client validation inspected a different installation state directory")
	}
}
