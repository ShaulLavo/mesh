package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/testenv"
)

func TestNamingCommandKeepsNativeSessionsIndependent(t *testing.T) {
	for _, command := range []string{"session-worker", "version", "update"} {
		if namingCommand([]string{command}) {
			t.Fatalf("native command reads deployment policy: %s", command)
		}
	}
	for _, command := range []string{"daemon", "serve", "unserve", "app", "private-names"} {
		if !namingCommand([]string{command}) {
			t.Fatalf("naming command misses deployment policy: %s", command)
		}
	}
}

func TestNamingCommandHandlesRootFlags(t *testing.T) {
	for _, args := range [][]string{{"--privacy", "serve"}, {"--leave-key", "ctrl+]", "app"}, {"--privacy=true", "daemon"}} {
		if !namingCommand(args) {
			t.Fatalf("missed naming command: %v", args)
		}
	}
	if !namingCommand([]string{"--privacy", "spawn", "serve"}) {
		t.Fatal("spawn skips deployment policy")
	}
}

func TestPickerLoadsDeploymentPolicy(t *testing.T) {
	for _, args := range [][]string{nil, {"--privacy"}, {"ls"}, {"spawn", "serve"}} {
		if !namingCommand(args) {
			t.Fatalf("entry skips deployment policy: %v", args)
		}
	}
}

func TestPickerEntryInitializesRealServiceCache(t *testing.T) {
	if os.Getenv("MESH_TEST_DOMAIN_PICKER") == "1" {
		verifyPickerServiceCache(t)
		return
	}
	root := t.TempDir()
	config := filepath.Join(root, "config")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "domains.json"), []byte(`{"primary":"mesh.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestPickerEntryInitializesRealServiceCache$") //nolint:gosec // execute the isolated test child without global policy fixtures
	child.Env = append(testenv.ForProcess(root), "MESH_TEST_DOMAIN_PICKER=1", "MESH_CONFIG_DIR="+config, "MESH_STATE_DIR="+filepath.Join(root, "state"))
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("picker cache child: %v\n%s", err, output)
	}
}

func verifyPickerServiceCache(t *testing.T) {
	t.Helper()
	if err := initializeDeployment(nil); err != nil {
		t.Fatal(err)
	}
	if domainpolicy.Primary() != "mesh.test" {
		t.Fatal("picker did not initialize its configured policy")
	}
	host, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache, err := cli.OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close() //nolint:errcheck // isolated test cache
	if err := cache.SaveServices(t.Context(), cli.HostRecord{ID: host.ID, MeshIdentity: host.ID}, "pc.mesh.mesh.test", []protocol.ServiceInfo{{Name: "site", Kind: "static", Target: t.TempDir(), PublicName: "blog.mesh.test", Healthy: true}}); err != nil {
		t.Fatal(err)
	}
}
