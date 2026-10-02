package update

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func LocalHost(stateDir, id string) Host {
	return Host{ID: id, Alias: "local", Endpoint: (&url.URL{Scheme: "unix", Path: filepath.Join(stateDir, "daemon.sock")}).String(), Platform: release.CurrentPlatform()}
}

func LocalProbe(stateDir string) updateinstall.Probe { return updatebootstrap.Probe(stateDir) }

func installedExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err = updateinstall.ValidateInstallationPath(executable); err != nil {
		return "", err
	}
	return executable, nil
}

func NewInstallation(stateDir string, clientOnly bool, probe updateinstall.Probe) (*updateinstall.Engine, error) {
	executable, err := installedExecutable()
	if err != nil {
		return nil, err
	}
	cache, err := CacheDir()
	if err != nil {
		return nil, err
	}
	config := updateinstall.Config{StateDir: stateDir, Executable: executable, CacheDir: cache, ClientOnly: clientOnly, Probe: probe, RequiredMount: os.Getenv("MESH_UPDATE_REQUIRED_MOUNT")}
	if !clientOnly {
		config.ServiceSpec, err = updateinstall.DefaultServiceSpec()
		if err != nil {
			return nil, err
		}
	}
	return updateinstall.New(config)
}

func EnsureHelper(ctx context.Context, stateDir string) (updateinstall.HelperInstallation, error) {
	executable, err := installedExecutable()
	if err != nil {
		return updateinstall.HelperInstallation{}, err
	}
	kind, domain := "systemd", ""
	if runtime.GOOS == "darwin" {
		kind = "launchd"
		domain = fmt.Sprintf("gui/%d", os.Getuid())
	}
	return updateinstall.InstallHelper(ctx, updateinstall.HelperConfig{StateDir: stateDir, Executable: executable, Kind: kind, Domain: domain})
}
