package bootstrap

import "testing"

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
