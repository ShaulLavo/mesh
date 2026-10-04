package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/usagefeed"
	"github.com/spf13/cobra"
)

func (a *application) dashboardCommand() *cobra.Command {
	var wall bool
	var theme string
	command := &cobra.Command{Use: "dashboard", Short: "Watch machine usage, sessions, and services", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("theme") {
				if err := ValidateDashboardTheme(theme); err != nil {
					return err
				}
			}
			return a.runDashboard(cmd.Context(), wall, theme)
		},
	}
	command.Flags().BoolVar(&wall, "wall", false, "show the passive fullscreen fleet view")
	command.Flags().StringVar(&theme, "theme", "", "dashboard theme ("+strings.Join(DashboardThemeNames(), ", ")+"); overrides config")
	return command
}
func (a *application) runDashboard(ctx context.Context, wall bool, override string) error {
	if a.dependencies.Dashboard == nil {
		return errors.New("dashboard terminal view is unavailable")
	}
	theme, err := dashboardConfiguredTheme(override)
	if err != nil {
		return err
	}
	records, localID, socket, err := dashboardInventory(ctx)
	if err != nil {
		return err
	}
	cache, err := OpenCatalogCache(ctx)
	if err != nil {
		return err
	}
	defer cache.Close() //nolint:errcheck // the view's result is authoritative
	dial := dashboardControlDialer(localID, socket, a.dependencies.DialControl)
	monitor := dashboardMonitor{records: records, localID: localID, watcher: NewStateWatcher(dial), cache: cache}
	input := DashboardInput{Privacy: a.privacy, Wall: wall, Theme: theme, Watch: monitor.Run}
	config, err := loadHostConfig()
	if err != nil {
		return err
	}
	if config.Dashboard != nil && config.Dashboard.UsageFeedURL != "" {
		feed, err := usagefeed.New(usagefeed.Config{URL: config.Dashboard.UsageFeedURL})
		if err != nil {
			return fmt.Errorf("dashboard usage feed: %w", err)
		}
		input.UsageWatch = feed.Run
	}
	for _, record := range records {
		input.Hosts = append(input.Hosts, dashboardHost(record, localID))
	}
	restart := dashboardRestart{current: release.Current(), argv: os.Args, env: os.Environ(),
		exec: func(ctx context.Context, build release.Build, path string, argv, env []string) error {
			return updateinstall.WithCommittedExecutable(ctx, filepath.Dir(socket), localID, build, func(installed string) error {
				if installed != path {
					return fmt.Errorf("installed mesh path changed before restart")
				}
				return syscall.Exec(installed, argv, env) //nolint:gosec // exec the verified installed image under the activation lock with unchanged argv and env
			})
		},
		installed: func(build release.Build) (string, error) {
			return dashboardInstalledTarget(filepath.Dir(socket), localID, build)
		},
	}
	return restart.run(ctx, input, func(run context.Context, next DashboardInput) error {
		operations := newPickerOperationGate()
		defer operations.stopAndWait()
		next.Inspect = dashboardInspector(records, dial, monitor.watcher, operations)
		return a.dependencies.Dashboard(run, next)
	})
}

func dashboardInspector(records []HostRecord, dial HostDialer, watcher *StateWatcher, operations *pickerOperationGate) PickerInspectFunc {
	return func(ctx context.Context, request PickerInspectRequest) (SessionInspection, error) {
		if !operations.begin(ctx) {
			return SessionInspection{}, context.Canceled
		}
		defer operations.done()
		host, err := resolveHostTarget(records, request.HostID)
		if err != nil {
			return SessionInspection{}, err
		}
		if err := watcher.acquire(ctx); err != nil {
			return SessionInspection{}, err
		}
		defer func() { <-watcher.reads }()
		return inspectRemoteSession(ctx, host, dial, request.SessionID, 1, 1)
	}
}
func dashboardInventory(ctx context.Context) ([]HostRecord, string, string, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return nil, "", "", err
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return nil, "", "", fmt.Errorf("dashboard state directory: %w", err)
	}
	local, err := identity.Load(stateDir)
	if err != nil {
		return nil, "", "", fmt.Errorf("dashboard local identity: %w", err)
	}
	seen := map[string]bool{}
	result := make([]HostRecord, 0, len(hosts)+1)
	for _, host := range hosts {
		if seen[host.ID] {
			continue
		}
		seen[host.ID] = true
		result = append(result, host)
	}
	localRecord := HostRecord{ID: local.ID, MeshIdentity: local.ID, local: true}
	readCtx, cancel := context.WithTimeout(ctx, localQueryTimeout)
	declaration, readErr := localDeclaredHost(readCtx, stateDir)
	cancel()
	if readErr == nil {
		if declaration.ID != local.ID || declaration.MeshIdentity != local.ID {
			return nil, "", "", fmt.Errorf("local daemon reported another identity")
		}
		localRecord = declaration
		localRecord.local = true
	}
	for i := range result {
		if result[i].ID == local.ID {
			result[i] = localRecord
		}
	}
	if !seen[local.ID] {
		result = append(result, localRecord)
	}
	ProjectHostNames(result)
	sort.SliceStable(result, func(i, j int) bool { return result[i].MachineName < result[j].MachineName })
	return result, local.ID, filepath.Join(stateDir, "daemon.sock"), nil
}
func dashboardHost(record HostRecord, localID string) DashboardHost {
	return DashboardHost{ID: record.ID, MachineName: record.MachineName, NameRevision: record.NameRevision, NameVerified: record.NameVerified, NameConflict: record.NameConflict, NamePriority: record.NamePriority, NameSuffix: record.NameSuffix, Local: record.ID == localID}
}
func dashboardControlDialer(localID, socket string, remote HostDialer) HostDialer {
	return func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		if host.ID != localID {
			return remote(ctx, host)
		}
		stream, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, fmt.Errorf("dashboard local daemon: %w", err)
		}
		conn, err := transport.NewStreamConn(stream)
		if err != nil {
			_ = stream.Close()
			return nil, fmt.Errorf("dashboard local transport: %w", err)
		}
		return conn, nil
	}
}
