package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

func resolverFrom(pairs map[string]string) sshConfigResolver {
	return func(alias, key string) string { return pairs[alias+"/"+key] }
}

func TestApplySSHConfigResolvesAnAlias(t *testing.T) {
	t.Parallel()

	resolve := resolverFrom(map[string]string{
		"pc/HostName": "10.168.63.60",
		"pc/User":     "shaul",
		"pc/Port":     "2200",
	})

	for _, row := range []struct {
		name   string
		raw    string
		host   string
		user   string
		port   uint16
		reason string
	}{
		{"alias alone takes every directive", "pc", "10.168.63.60", "shaul", 2200,
			"ssh pc resolves all three, so mesh add pc must too"},
		{"an explicit user wins", "root@pc", "10.168.63.60", "root", 2200,
			"the command line beats the config file, as it does for ssh"},
		{"an explicit port wins", "pc:22", "10.168.63.60", "shaul", 22,
			"same rule for the port"},
		{"an unknown alias is untouched", "me@other", "other", "me", 22,
			"a host with no Host block must dial exactly what was typed"},
	} {
		parsed, err := parseTarget(row.raw)
		if err != nil {
			t.Fatalf("%s: parse %q: %v", row.name, row.raw, err)
		}
		got := applySSHConfig(parsed, resolve)
		if got.host != row.host || got.user != row.user || got.port != row.port {
			t.Errorf("%s (%s): %q -> %s@%s:%d, want %s@%s:%d",
				row.name, row.reason, row.raw,
				got.user, got.host, got.port, row.user, row.host, row.port)
		}
	}
}

func TestApplySSHConfigWithoutAConfigChangesNothing(t *testing.T) {
	t.Parallel()

	parsed, err := parseTarget("me@10.0.0.4:2222")
	if err != nil {
		t.Fatal(err)
	}
	// A machine with no ssh config must still adopt by address.
	got := applySSHConfig(parsed, nil)
	if got.user != "me" || got.host != "10.0.0.4" || got.port != 2222 {
		t.Fatalf("nil resolver changed the target: %s@%s:%d", got.user, got.host, got.port)
	}
}

func TestUserPrecedenceMatchesSSH(t *testing.T) {
	t.Parallel()

	// Command line beats config beats local user, the same order ssh uses.
	resolve := resolverFrom(map[string]string{
		"pi/HostName": "10.0.0.9",
		"pi/User":     "pi",
	})
	for _, row := range []struct {
		name string
		raw  string
		want string
	}{
		{"the config names the user for a bare alias", "pi", "pi"},
		{"an explicit user still wins", "root@pi", "root"},
	} {
		parsed, err := parseTarget(row.raw)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if got := applySSHConfig(parsed, resolve).user; got != row.want {
			t.Errorf("%s: user = %q, want %q", row.name, got, row.want)
		}
	}

	// A host the config says nothing about is left for the local-user fallback.
	parsed, err := parseTarget("unknown-host")
	if err != nil {
		t.Fatal(err)
	}
	if got := applySSHConfig(parsed, resolve).user; got != "" {
		t.Fatalf("user = %q, want it left empty for the caller's fallback", got)
	}
}

func TestConfiguredUserHintExplainsAnOverride(t *testing.T) {
	t.Parallel()

	// Only when a user was typed and the config disagrees.
	quiet, err := parseTarget("pi")
	if err != nil {
		t.Fatal(err)
	}
	if hint := configuredUserHint(quiet); hint != "" {
		t.Fatalf("hint for a bare alias = %q, want none", hint)
	}
}

func TestSSHConfigIdentityFileIsUsed(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	resolve := resolverFrom(map[string]string{
		"pc/IdentityFile":       "~/.ssh/id_ed25519_pc",
		"absolute/IdentityFile": "/keys/absolute",
	})

	// ssh accepts ~ in IdentityFile; os.Open does not.
	if got := sshConfigIdentityFile("pc", resolve); got != filepath.Join(home, ".ssh", "id_ed25519_pc") {
		t.Fatalf("identity = %q, want the expanded home path", got)
	}
	if got := sshConfigIdentityFile("absolute", resolve); got != "/keys/absolute" {
		t.Fatalf("identity = %q, want it untouched", got)
	}
	// A host the config says nothing about leaves the default key names alone.
	if got := sshConfigIdentityFile("unknown", resolve); got != "" {
		t.Fatalf("identity = %q, want none", got)
	}
	if got := sshConfigIdentityFile("pc", nil); got != "" {
		t.Fatalf("identity with no config = %q, want none", got)
	}
}
