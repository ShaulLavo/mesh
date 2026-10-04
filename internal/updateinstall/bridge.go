package updateinstall

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
)

// ReviewBridge requires settled ownership and a running verified helper. It
// neither installs a helper nor repairs an interrupted promotion or rollback.
func ReviewBridge(ctx context.Context, stateDir, executable string, health Health, manifest release.Manifest, probe func(context.Context, HelperInstallation) (int, error)) error {
	settings, err := reviewBridgeInstallation(ctx, stateDir, executable, health, manifest)
	if err != nil {
		return err
	}
	installed, err := reviewBridgeHelper(ctx, stateDir, settings.Service, manifest)
	if err != nil {
		return err
	}
	if probe == nil {
		return errors.New("bridge requires a running helper image probe")
	}
	if _, err := probe(ctx, installed); err != nil {
		return fmt.Errorf("verify running bridge helper: %w", err)
	}
	return nil
}

func reviewBridgeInstallation(ctx context.Context, stateDir, executable string, health Health, manifest release.Manifest) (Settings, error) {
	status, err := ReadContext(ctx, stateDir)
	if err != nil {
		return Settings{}, fmt.Errorf("read bridge installation ownership: %w", err)
	}
	if !finished(status.Phase) || status.Phase == RollbackFailed {
		return Settings{}, errors.New("bridge installation needs recovery before another update")
	}
	settings := status.Settings
	if status.Request.TargetID != health.HostID || settings.StateDir != stateDir || settings.Executable != executable || settings.ClientOnly || health.Build.Platform != release.CurrentPlatform() {
		return Settings{}, errors.New("bridge requires this daemon's existing installation and service ownership")
	}
	if err := manifest.Allows(health.Build); err != nil {
		return Settings{}, fmt.Errorf("check bridge source compatibility: %w", err)
	}
	if err := compatibleWorkers(health, manifest.Compatibility); err != nil {
		return Settings{}, err
	}
	if err := ValidateInstallationPath(executable); err != nil {
		return Settings{}, err
	}
	if err := release.VerifyExecutable(ctx, executable, health.Build.Digest); err != nil {
		return Settings{}, fmt.Errorf("verify installed bridge source: %w", err)
	}
	if err := checkMount(settings.RequiredMount); err != nil {
		return Settings{}, err
	}
	service, err := NewSystemService(settings.Service)
	if err != nil {
		return Settings{}, err
	}
	if err := service.Preflight(ctx); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func reviewBridgeHelper(ctx context.Context, stateDir string, service ServiceSpec, manifest release.Manifest) (HelperInstallation, error) {
	cfg := HelperConfig{StateDir: stateDir, Kind: service.Kind, Domain: service.Domain}
	var installed HelperInstallation
	if err := readHelperInstallation(ctx, stateDir, &installed); err != nil {
		return HelperInstallation{}, err
	}
	cfg.ServiceDir = filepath.Dir(installed.ServicePath)
	snapshot, err := recoveryHelperSnapshot(ctx, cfg)
	if err != nil {
		return HelperInstallation{}, err
	}
	build, err := helperBuild(ctx, snapshot.installation.Executable, snapshot.installation.Digest)
	if err != nil {
		return HelperInstallation{}, err
	}
	if err := helperCapabilities(build, manifest); err != nil {
		return HelperInstallation{}, err
	}
	order, err := release.CompareVersions(build.Version, manifest.Version)
	if err != nil || order < 0 {
		return HelperInstallation{}, errors.Join(err, errors.New("bridge requires a recovered helper at least as new as the next hop"))
	}
	if _, err := helperCommandOutput(ctx, installed.Executable, "update-helper", "--state-dir", stateDir, "--check-journal"); err != nil {
		return HelperInstallation{}, fmt.Errorf("check bridge helper journal support: %w", err)
	}
	return installed, nil
}
