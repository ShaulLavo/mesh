package bootstrap

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh/knownhosts"
)

func TestSystemSSHUsesConfiguredAccountKeyAndKnownHosts(t *testing.T) {
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
