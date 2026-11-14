package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestInstallHelperConfigPreservesStateAndAccountDomain(t *testing.T) {
	state := filepath.Join(t.TempDir(), "custom-state")
	t.Setenv("MESH_STATE_DIR", state)
	domain := "user/" + strconv.Itoa(os.Getuid())
	cfg, err := installHelperConfig(helperLaunchd, domain)
	if err != nil || cfg.StateDir != state || cfg.Domain != domain || cfg.Probe == nil || cfg.State == nil {
		t.Fatalf("helper did not bind state and account: %+v, %v", cfg, err)
	}
	if _, err := installHelperConfig(helperLaunchd, "gui/99999999"); err == nil {
		t.Fatal("helper accepted another account's domain")
	}
	if _, err := installHelperConfig(helperSystemd, domain); err == nil {
		t.Fatal("systemd helper accepted a launchd domain")
	}
	if _, err := installHelperConfig("command", ""); err == nil {
		t.Fatal("helper accepted an unsupported service manager")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("configuration lookup created installation state: %v", err)
	}
}

func TestDevelopmentInstallerLeavesUpdaterUnconfigured(t *testing.T) {
	build := release.Current()
	if _, err := release.CompareVersions(build.Version, build.Version); err == nil && !build.Modified {
		t.Fatal("development control unexpectedly has an unmodified release identity")
	}
	state := filepath.Join(t.TempDir(), "development-state")
	t.Setenv("MESH_STATE_DIR", state)
	if err := prepareInstallUpdates(t.Context(), helperSystemd, ""); err != nil {
		t.Fatal(err)
	}
	if err := checkInstalledHelper(t.Context(), helperSystemd, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("development install created helper, policy or trust state: %v", err)
	}
}
