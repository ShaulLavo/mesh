package cli

import (
	"github.com/shaul/mesh/internal/machinename"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHostIDRejectsSessionIDs(t *testing.T) {
	_, err := machinename.Normalize("7k3d")
	if err == nil || !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("machinename.Normalize(7k3d) error = %v, want session ID explanation", err)
	}
}

func TestValidateHostIDRejectsReservedCommands(t *testing.T) {
	for _, alias := range []string{"private-names", "serve", "unserve"} {
		if _, err := machinename.Normalize(alias); err == nil || !strings.Contains(err.Error(), "Mesh command") {
			t.Fatalf("%s alias error = %v", alias, err)
		}
	}
}

func TestHostConfigRoundTripReplacesSameHostAtomically(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", configDir)

	first := HostRecord{
		MachineName:   "pc",
		ID:            "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE",
		MeshIdentity:  "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE",
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

func TestSessionShapedInputRemainsASessionTarget(t *testing.T) {
	target, err := ResolveArgument("7k3d", nil)
	if err != nil || target.SessionID != "7K3D" || target.Host != nil {
		t.Fatalf("session argument = %+v, %v", target, err)
	}
}

func TestResolveArgumentNamesBothPossibilitiesOnMiss(t *testing.T) {
	_, err := ResolveArgument("missing", []HostRecord{{MachineName: "pc"}})
	if err == nil || !strings.Contains(err.Error(), "exact host ID") || !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("ResolveArgument miss error = %v", err)
	}
}
