package bootstrap

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh/knownhosts"
)

func TestSystemSSHUsesConfiguredAccountKeyAndKnownHosts(t *testing.T) {
	path := configureSystemSSHFixture(t)
	stdout, stderr, err := RunSystemSSH(t.Context(), "adoption-fixture", "uname -s", nil)
	if err != nil || string(stdout) != "ran:uname -s" || len(stderr) != 0 {
		t.Fatalf("configured system SSH: %s %s %v", stdout, stderr, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunSystemSSH(t.Context(), "adoption-fixture", "uname -s", nil); err == nil {
		t.Fatal("unknown host was accepted without a prompt")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("noninteractive SSH changed known hosts")
	}
}

func TestSystemSSHFailureKeepsRemoteStderr(t *testing.T) {
	configureSystemSSHFixture(t)
	_, stderr, err := RunSystemSSH(t.Context(), "adoption-fixture", "fail-with-stderr", nil)
	if err == nil {
		t.Fatal("failing remote command reported success")
	}
	want := "adoption: host updater: inspect daemon service using plutil: exit status 1"
	if !strings.Contains(err.Error(), want) || !strings.Contains(string(stderr), want) {
		t.Fatalf("remote stderr missing from failure: %v", err)
	}
	if strings.Contains(err.Error(), "partial") {
		t.Fatalf("stdout replaced the remote stderr: %v", err)
	}
}

// configureSystemSSHFixture points the account's SSH config at a test server
// under the alias adoption-fixture and returns its known-hosts file.
func configureSystemSSHFixture(t *testing.T) string {
	t.Helper()
	private, public := generateSSHKey(t)
	server := startSSHTestServer(t, public)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(server.address)
	if err != nil {
		t.Fatal(err)
	}
	key := writeSSHIdentity(t, private)
	config := fmt.Sprintf("Host adoption-fixture\n  HostName %s\n  Port %s\n  User configured-account\n  IdentityFile %s\n  UserKnownHostsFile ~/.ssh/adoption_known_hosts\n", host, port, key)
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	known := knownhosts.Line([]string{knownhosts.Normalize(server.address)}, server.hostKey) + "\n"
	path := filepath.Join(sshDir, "adoption_known_hosts")
	if err := os.WriteFile(path, []byte(known), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
