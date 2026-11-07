package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/testenv"
)

func TestNamingCommandKeepsNativeSessionsIndependent(t *testing.T) {
	for _, command := range []string{"session-worker", "version", "update", "device"} {
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
	if namingCommand([]string{"--privacy", "device", "approve-checked"}) {
		t.Fatal("native enrollment reads deployment policy behind root flags")
	}
	if !namingCommand([]string{"--privacy", "spawn", "serve"}) {
		t.Fatal("spawn skips deployment policy")
	}
}

func TestPickerLoadsDeploymentPolicy(t *testing.T) {
	for _, args := range [][]string{nil, {"--privacy"}, {"ls"}, {"list"}, {"spawn", "serve"}} {
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
	child := exec.CommandContext(t.Context(), "/usr/bin/env", executable, "-test.run=^TestPickerEntryInitializesRealServiceCache$") //nolint:gosec // execute the isolated test child without global policy fixtures
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
	if _, err := cache.SaveServices(t.Context(), cli.HostRecord{ID: host.ID, MeshIdentity: host.ID}, "pc.mesh.mesh.test", []protocol.ServiceInfo{{Name: "site", Kind: "static", Target: t.TempDir(), Healthy: true}}); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateAppDaemonRequestsDeploymentPolicy(t *testing.T) {
	for _, flag := range []string{"--app-registry-config", "--app-registry-target"} {
		for _, args := range [][]string{{"daemon", flag, "/tmp/config.json"}, {"daemon", flag + "=/tmp/config.json"}} {
			if !deploymentRequested(args) {
				t.Fatalf("private app role skips policy: %v", args)
			}
		}
	}
	if deploymentRequested([]string{"daemon", "--edge=/tmp/config.json"}) {
		t.Fatal("removed public role initializes deployment")
	}
}

func TestReadOnlyCommandEntryDoesNotPublishDomainState(t *testing.T) {
	if os.Getenv("MESH_READONLY_ENTRY_CHILD") == "1" {
		verifyReadOnlyCommandEntry(t)
		return
	}
	for _, command := range []string{"ssh", "spawn", "attach", "ls", "picker"} {
		t.Run(command, func(t *testing.T) { runReadOnlyCommandEntry(t, command) })
	}
}

func runReadOnlyCommandEntry(t *testing.T, command string) {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(root, "config")
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "hosts.json"), []byte(`{"version":1,"hosts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), "/proc/self/exe", "-test.run=^TestReadOnlyCommandEntryDoesNotPublishDomainState$")
	if runtime.GOOS != "linux" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child.Path, child.Args[0] = executable, executable
	}
	child.Env = append(testenv.ForProcess(root), "MESH_READONLY_ENTRY_CHILD=1", "MESH_READONLY_ENTRY_COMMAND="+command, "MESH_CONFIG_DIR="+config, "MESH_STATE_DIR="+filepath.Join(root, "state"))
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("read-only entry child: %v\n%s", err, output)
	}
	for _, path := range []string{filepath.Join(root, "state"), filepath.Join(config, "domains.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only entry created deployment state %s: %v", path, err)
		}
	}
}

func verifyReadOnlyCommandEntry(t *testing.T) {
	t.Helper()
	args := []string{os.Getenv("MESH_READONLY_ENTRY_COMMAND")}
	if args[0] == "picker" {
		args = nil
	}
	if err := initializeDeployment(args); err != nil {
		t.Fatal(err)
	}
	if domainpolicy.Primary() == "" {
		t.Fatal("read-only entry did not infer the existing deployment policy")
	}
}
