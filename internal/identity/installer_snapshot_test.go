package identity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	installscript "github.com/shaul/mesh/scripts/install"
	"golang.org/x/crypto/ssh"
)

func TestInstallerRejectsUnsafePolicyWithoutSnapshot(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			checkInstallerPolicyKinds(t, platform)
		})
	}
}

func checkInstallerPolicyKinds(t *testing.T, platform string) {
	t.Helper()
	for _, kind := range []string{"symlink", "oversized", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			checkInstallerRejectedSnapshot(t, platform, kind)
		})
	}
}

func checkInstallerRejectedSnapshot(t *testing.T, platform, kind string) {
	t.Helper()
	home, state, bin, script := installerGrantFixture(t)
	if platform == "darwin" {
		var ok bool
		script, ok = installscript.Script(platform)
		if !ok {
			t.Fatal("Darwin installer is missing")
		}
		if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil { //nolint:gosec // fixture-owned service manager
			t.Fatal(err)
		}
	}
	_, key, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "unrelated")
	contents := bytes.Repeat([]byte("unrelated fixture data\n"), 110000)
	if err = os.WriteFile(target, contents, 0600); err != nil { //nolint:gosec // fixture-owned unrelated file
		t.Fatal(err)
	}
	policy := filepath.Join(state, "authorized_keys")
	switch kind {
	case "symlink":
		err = os.Symlink(target, policy)
	case "oversized":
		err = os.WriteFile(policy, contents, 0600) //nolint:gosec // fixture-owned oversized policy
	case "malformed":
		err = os.WriteFile(policy, []byte("malformed fixture key\n"), 0600) //nolint:gosec // fixture-owned malformed policy
	}
	if err != nil {
		t.Fatal(err)
	}
	output, commandErr := installerPlatformGrantCommand(t, platform, home, bin, script, public).CombinedOutput()
	if commandErr == nil {
		t.Fatalf("installer accepted unsafe %s policy: %s", kind, output)
	}
	if !strings.Contains(string(output), "cannot approve the adopter device key") {
		t.Fatalf("installer failed outside the managed approval boundary: %s", output)
	}
	if kind == "symlink" {
		link, err := os.Readlink(policy)
		if err != nil || link != target {
			t.Fatalf("installer changed rejected policy symlink: %q, %v", link, err)
		}
	}
	entries, err := filepath.Glob(filepath.Join(state, ".authorized_keys.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected policy left activation snapshots: %v", entries)
	}
	actual, err := os.ReadFile(target) //nolint:gosec // fixture-owned unrelated file
	if err != nil || !bytes.Equal(actual, contents) {
		t.Fatalf("installer changed unrelated policy target: %v", err)
	}
}
