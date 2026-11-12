package update

import (
	"context"
	"errors"
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
	return Host{ID: id, Endpoint: (&url.URL{Scheme: "unix", Path: filepath.Join(stateDir, "daemon.sock")}).String(), Platform: release.CurrentPlatform()}
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
	cfg := updateinstall.HelperConfig{StateDir: stateDir, Executable: executable, Kind: kind, Domain: domain}
	cfg.Probe = updatebootstrap.HelperProcessProbe(cfg)
	cfg.State = func(ctx context.Context) (updateinstall.HelperProcessState, error) {
		return updatebootstrap.HelperServiceState(ctx, cfg)
	}
	return ensureLocalHelper(ctx, cfg)
}

func ensureLocalHelper(ctx context.Context, cfg updateinstall.HelperConfig) (updateinstall.HelperInstallation, error) {
	status, err := updateinstall.ReadContext(ctx, cfg.StateDir)
	if errors.Is(err, os.ErrNotExist) {
		installed, installErr := updateinstall.InstallHelper(ctx, cfg)
		return installed, localHelperError("initialize", installErr)
	}
	if err != nil {
		return updateinstall.HelperInstallation{}, fmt.Errorf("read helper installation journal: %w", err)
	}
	if status.Settings.StateDir != cfg.StateDir || status.Settings.Executable != cfg.Executable {
		return updateinstall.HelperInstallation{}, errors.New("helper journal belongs to another installation")
	}
	if !status.Settings.ClientOnly {
		cfg.Kind, cfg.Domain = status.Settings.Service.Kind, status.Settings.Service.Domain
		cfg.Probe = updatebootstrap.HelperProcessProbe(cfg)
		cfg.State = func(ctx context.Context) (updateinstall.HelperProcessState, error) {
			return updatebootstrap.HelperServiceState(ctx, cfg)
		}
	}
	switch status.Phase {
	case updateinstall.Accepted, updateinstall.Staged, updateinstall.Granted, updateinstall.Activating,
		updateinstall.Validating, updateinstall.RollingBack, updateinstall.RolledBack, updateinstall.RollbackFailed,
		updateinstall.Failed, updateinstall.Cancelled:
		installed, inspectErr := updateinstall.InspectHelper(ctx, cfg)
		return installed, localHelperError("inspect", inspectErr)
	case updateinstall.Committed:
	default:
		return updateinstall.HelperInstallation{}, errors.New("unsupported helper installation journal phase")
	}
	artifact, err := status.Request.Manifest.Artifact(release.CurrentPlatform())
	if err != nil {
		return updateinstall.HelperInstallation{}, fmt.Errorf("read committed helper binding: %w", err)
	}
	if err = release.VerifyExecutable(ctx, cfg.Executable, artifact.BinarySHA256); err != nil {
		return updateinstall.HelperInstallation{}, fmt.Errorf("verify committed installed executable: %w", err)
	}
	installed, err := updateinstall.EnsureCurrentHelper(ctx, cfg, status.Request.Manifest)
	return installed, localHelperError("ensure current", err)
}

func localHelperError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s update helper: %w", action, err)
}
