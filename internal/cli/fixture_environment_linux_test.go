package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/testenv"
)

func TestCommandFixturesIgnoreInheritedMeshDirectories(t *testing.T) {
	for _, mode := range []string{"mesh", "xdg"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			checkInheritedCommandEnvironment(t, mode)
		})
	}
}

func TestCommandFixtureEnvironmentPreservesPreparedDirectories(t *testing.T) {
	prepareCommandEnvironment(t)
	checkPreparedCommandDirectories(t)
	t.Run("explicit overrides", func(t *testing.T) {
		t.Setenv("MESH_CONFIG_DIR", t.TempDir())
		t.Setenv("MESH_STATE_DIR", t.TempDir())
		checkPreparedCommandDirectories(t)
	})
}

func checkPreparedCommandDirectories(t *testing.T) {
	t.Helper()
	config, state := os.Getenv("MESH_CONFIG_DIR"), os.Getenv("MESH_STATE_DIR")
	contents := []byte("prepared fixture data")
	roots := make([]*os.Root, 0, 2)
	for _, directory := range []string{config, state} {
		root, err := os.OpenRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Close() })
		roots = append(roots, root)
		if err := root.WriteFile("prepared", contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prepareCommandEnvironment(t)
	if os.Getenv("MESH_CONFIG_DIR") != config || os.Getenv("MESH_STATE_DIR") != state {
		t.Fatal("environment preparation replaced test-owned directories")
	}
	for _, root := range roots {
		actual, err := root.ReadFile("prepared")
		if err != nil || !bytes.Equal(actual, contents) {
			t.Fatalf("prepared data changed: %q, %v", actual, err)
		}
	}
}

func checkInheritedCommandEnvironment(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(root, "mesh")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	poison := []byte(`{"primary":"outside.example","aliases":[],"legacyCertificateDomain":"outside.example"}`)
	policy := filepath.Join(config, "domains.json")
	if err := os.WriteFile(policy, poison, 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), "/proc/self/exe", "-test.run=^(TestAgentDecodedServiceGoldens|TestCatalogServiceWriterContentionSkipsAtomicSnapshot|TestDomainGenerationSnapshotCloneAndQueuedResult|TestServeCommandPreviewsThenPublishesCanonicalService|TestPickerServiceActionsRouteToExactHostAndSettle|TestPickerServiceOpenUsesInvokingMachineURL|TestPickerServicePingUnhealthyIsASettledResult|TestPickerFleetServicesWatchRetainsOfflineCacheAndReportsFailure|TestRemoteServiceTransportErrorsAreBoundedAndPreserveCause|TestRemoteServiceBoundaryRejectsChangedPreviewAndAcknowledgement|TestRemoteServiceBoundaryPinsInferredKindAndCanonicalPort|TestRemoteServiceBoundaryDoesNotEchoOversizedInvalidFields)$")
	child.Env = append(testenv.ForProcess(root), "MESH_STATE_DIR="+filepath.Join(root, "outside-state"))
	if mode == "mesh" {
		child.Env = append(child.Env, "MESH_CONFIG_DIR="+config)
	} else {
		child.Env = append(child.Env, "XDG_CONFIG_HOME="+root)
	}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("inherited %s configuration affected CLI fixtures: %v\n%s", mode, err, output)
	}
	outside, err := os.OpenRoot(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outside.Close() })
	contents, err := outside.ReadFile("domains.json")
	if err != nil || !bytes.Equal(contents, poison) {
		t.Fatalf("outside policy changed: %q, %v", contents, err)
	}
	entries, err := os.ReadDir(config)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outside configuration gained fixture files: %v, %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(root, "outside-state")); !os.IsNotExist(err) {
		t.Fatalf("fixture used outside state: %v", err)
	}
}
