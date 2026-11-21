package updatebootstrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The bootstrap download must stage in the cache the daemon uses. On macOS
// that is ~/Library/Caches even when XDG_CACHE_HOME is set; the script used to
// create ~/.cache instead.
func TestBootstrapDownloadUsesTheDaemonCache(t *testing.T) {
	shells := []string{"/bin/sh"}
	if dash, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, dash)
	}
	cases := []struct{ uname, want string }{
		{"Darwin", filepath.Join("Library", "Caches", "mesh", "updates")},
		{"Linux", filepath.Join("xdg", "mesh", "updates")},
	}
	for _, shell := range shells {
		for _, c := range cases {
			t.Run(filepath.Base(shell)+"/"+c.uname, func(t *testing.T) {
				home, bin := t.TempDir(), t.TempDir()
				tool := func(name, body string) {
					if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // isolated tool stub
						t.Fatal(err)
					}
				}
				tool("uname", "case \"$1\" in -s) echo "+c.uname+" ;; -m) echo arm64 ;; esac\n")
				tool("curl", "printf '%s\\n' 'fixture curl reached' >&2\nexit 7\n")
				command := exec.CommandContext(t.Context(), shell, "-c", downloadScript, "mesh-update-bootstrap", "op", "v0.2.0", "request", "a", "b", "c") //nolint:gosec // fixed bootstrap script in an isolated account
				command.Env = []string{"HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, "xdg"), "PATH=" + bin + ":/usr/bin:/bin"}
				output, err := command.CombinedOutput()
				if err == nil || !strings.Contains(string(output), "fixture curl reached") {
					t.Fatalf("download did not reach curl: %s %v", output, err)
				}
				entries, err := os.ReadDir(filepath.Join(home, c.want))
				if err != nil || len(entries) != 0 {
					t.Fatalf("cache %s: %v %v", c.want, entries, err)
				}
				if c.uname == "Darwin" {
					if _, err := os.Stat(filepath.Join(home, "xdg")); !os.IsNotExist(err) {
						t.Fatalf("macOS bootstrap wrote under XDG_CACHE_HOME: %v", err)
					}
				}
			})
		}
	}
}
