package install

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinServiceChildrenPreferManagedCommand(t *testing.T) {
	home := t.TempDir()
	managedDir := filepath.Join(home, ".local", "bin")
	brewDir := filepath.Join(home, "homebrew", "bin")
	for _, dir := range []string{managedDir, brewDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	managed := "#!/bin/sh\ncommand -v mesh\n"
	writeDarwinFixture(t, filepath.Join(managedDir, "mesh"), managed, 0700)
	writeDarwinFixture(t, filepath.Join(brewDir, "mesh"), "#!/bin/sh\nexit 99\n", 0700)
	service, err := RenderService("darwin", ServiceOptions{DaemonPort: 7337, SSHPort: 2222, WebSocketPath: "/mesh"})
	if err != nil {
		t.Fatal(err)
	}
	var plist struct {
		Strings []string `xml:"dict>array>string"`
	}
	if err := xml.Unmarshal([]byte(service), &plist); err != nil || len(plist.Strings) != 3 {
		t.Fatalf("service arguments = %+v, %v", plist, err)
	}
	command := strings.Split(plist.Strings[2], " >>")[0]
	run := exec.CommandContext(t.Context(), "/bin/sh", "-c", command) //nolint:gosec // executes this checkout's rendered service against isolated fixture commands
	run.Env = []string{"HOME=" + home, "PATH=" + brewDir + ":/usr/bin:/bin"}
	output, err := run.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != filepath.Join(managedDir, "mesh") {
		t.Fatalf("managed daemon resolved another mesh: %s, %v", output, err)
	}
}
