package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/spf13/cobra"
)

func prepareInstallUpdatesCommand() *cobra.Command {
	var kind, domain string
	command := &cobra.Command{Use: "prepare-updates", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return prepareInstallUpdates(cmd.Context(), kind, domain)
		},
	}
	command.Flags().StringVar(&kind, "service-kind", "", "daemon service manager")
	command.Flags().StringVar(&domain, "domain", "", "daemon launchd domain")
	return command
}

func installHelperConfig(kind, domain string) (updateinstall.HelperConfig, error) {
	stateDir, err := paths.StateDirPath()
	if err != nil {
		return updateinstall.HelperConfig{}, fmt.Errorf("locate installation state: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return updateinstall.HelperConfig{}, fmt.Errorf("locate installed executable: %w", err)
	}
	if kind != helperSystemd && kind != helperLaunchd {
		return updateinstall.HelperConfig{}, errors.New("installation requires a systemd or launchd service")
	}
	uid := strconv.Itoa(os.Getuid())
	if kind == helperLaunchd && domain != "gui/"+uid && domain != "user/"+uid || kind == helperSystemd && domain != "" {
		return updateinstall.HelperConfig{}, errors.New("installation helper must use this account's daemon service domain")
	}
	cfg := updateinstall.HelperConfig{StateDir: stateDir, Executable: executable, Kind: kind, Domain: domain}
	cfg.Probe = updatebootstrap.HelperProcessProbe(cfg)
	cfg.State = func(ctx context.Context) (updateinstall.HelperProcessState, error) {
		return updatebootstrap.HelperServiceState(ctx, cfg)
	}
	return cfg, nil
}

func prepareInstallUpdates(ctx context.Context, kind, domain string) error {
	cfg, err := installHelperConfig(kind, domain)
	if err != nil {
		return err
	}
	build := release.Current()
	installation := updateinstall.InstallationKind(cfg.Executable, build, true)
	if installation == updateinstall.InstallationDevelopment {
		return nil
	}
	if installation != updateinstall.InstallationManaged {
		return errors.New("automatic updates require a Mesh-managed daemon executable")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	source, _, err := update.OfficialHostSource(cfg.StateDir)(ctx, build)
	if err != nil {
		return fmt.Errorf("authenticate installed release before starting its helper: %w", err)
	}
	if _, err := updateinstall.EnsureInstalledHelper(ctx, cfg, source.Manifest); err != nil {
		return fmt.Errorf("prepare installed update helper: %w", err)
	}
	return nil
}

func checkInstalledHelper(ctx context.Context, kind, domain string) error {
	if kind == "" {
		return nil
	}
	cfg, err := installHelperConfig(kind, domain)
	if err != nil {
		return err
	}
	if updateinstall.InstallationKind(cfg.Executable, release.Current(), true) == updateinstall.InstallationDevelopment {
		return nil
	}
	installed, err := updateinstall.InspectHelper(ctx, cfg)
	if err != nil {
		return fmt.Errorf("inspect installed update helper: %w", err)
	}
	pid, err := cfg.Probe(ctx, installed)
	if err != nil || pid <= 0 {
		return errors.Join(errors.New("installed update helper has no verified running image"), err)
	}
	confirmed, err := cfg.Probe(ctx, installed)
	if err != nil || confirmed != pid {
		return errors.Join(errors.New("installed update helper changed during readiness check"), err)
	}
	return nil
}
