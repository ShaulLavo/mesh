package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHostAliasRejectsSessionIDs(t *testing.T) {
	_, err := ValidateHostAlias("7k3d")
	if err == nil || !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("ValidateHostAlias(7k3d) error = %v, want session ID explanation", err)
	}
}

func TestValidateHostAliasRejectsPrivateNamesCommand(t *testing.T) {
	if _, err := ValidateHostAlias("private-names"); err == nil || !strings.Contains(err.Error(), "Mesh command") {
		t.Fatalf("private-names alias error = %v", err)
	}
}

func TestHostConfigRoundTripReplacesSameHostAtomically(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", configDir)

	first := HostRecord{
		Alias:         "pc",
		ID:            "host-key",
		MeshIdentity:  "host-key",
		TailscaleName: "pc.tail.example",
		Addresses:     []string{"100.64.0.2"},
		Endpoint:      "ws://100.64.0.2:7777/mesh",
	}
	if err := SaveHost(first); err != nil {
		t.Fatal(err)
	}
	first.Endpoint = "ws://100.64.0.3:7777/mesh"
	first.Addresses = []string{"100.64.0.3"}
	if err := SaveHost(first); err != nil {
		t.Fatal(err)
	}

	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Endpoint != first.Endpoint || hosts[0].Addresses[0] != "100.64.0.3" {
		t.Fatalf("hosts = %#v, want updated host", hosts)
	}
	path := filepath.Join(configDir, "hosts.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("hosts.json permissions = %04o, want 0600", got)
	}
}

func TestResolveArgumentReportsHostSessionAmbiguity(t *testing.T) {
	_, err := ResolveArgument("7k3d", []HostRecord{{Alias: "7k3d"}})
	if err == nil || !strings.Contains(err.Error(), "host alias") || !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("ResolveArgument collision error = %v", err)
	}
}

func TestResolveArgumentNamesBothPossibilitiesOnMiss(t *testing.T) {
	_, err := ResolveArgument("missing", []HostRecord{{Alias: "pc"}})
	if err == nil || !strings.Contains(err.Error(), "host alias") || !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("ResolveArgument miss error = %v", err)
	}
}
