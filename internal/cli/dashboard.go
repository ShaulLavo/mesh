package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/transport"
	"github.com/spf13/cobra"
)

func (a *application) dashboardCommand() *cobra.Command {
	var wall bool
	command := &cobra.Command{Use: "dashboard", Short: "Watch machine usage, sessions, and services", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runDashboard(cmd.Context(), wall) },
	}
	command.Flags().BoolVar(&wall, "wall", false, "show the passive fullscreen fleet view")
	return command
}
func (a *application) runDashboard(ctx context.Context, wall bool) error {
	if a.dependencies.Dashboard == nil {
		return errors.New("dashboard terminal view is unavailable")
	}
	records, localID, socket, err := dashboardInventory()
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
	input := DashboardInput{Wall: wall, Watch: monitor.Run}
	for _, record := range records {
		input.Hosts = append(input.Hosts, dashboardHost(record, localID))
	}
	return a.dependencies.Dashboard(ctx, input)
}
func dashboardInventory() ([]HostRecord, string, string, error) {
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
	if !seen[local.ID] {
		name, err := os.Hostname()
		if err != nil {
			return nil, "", "", fmt.Errorf("dashboard local hostname: %w", err)
		}
		result = append(result, HostRecord{Alias: name, ID: local.ID, MeshIdentity: local.ID})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Alias < result[j].Alias })
	return result, local.ID, filepath.Join(stateDir, "daemon.sock"), nil
}
func dashboardHost(record HostRecord, localID string) DashboardHost {
	return DashboardHost{ID: record.ID, Alias: dashboardText(record.Alias), Local: record.ID == localID}
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
