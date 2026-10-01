package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

const (
	dashboardReadTimeout = 1500 * time.Millisecond
	dashboardMetricsInterval = 2 * time.Second
	dashboardCatalogInterval = 10 * time.Second
	dashboardConcurrency = 4
	dashboardInspectionLimit = 6
)

func (a *application) dashboardCommand() *cobra.Command {
	var wall bool
	command := &cobra.Command{
		Use: "dashboard",
		Short: "Watch machine usage, sessions, and services without waking hosts",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDashboard(cmd.Context(), wall)
		},
	}
	command.Flags().BoolVar(&wall, "wall", false, "show the passive fullscreen TV layout")
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
	monitor := newDashboardMonitor(records, localID, a.dependencies.DialControl, a.dependencies.Now)
	monitor.cache = cache
	monitor.localSocket = socket
	monitor.loadCache(ctx)
	input := DashboardInput{Wall: wall, Watch: monitor.Run}
	for _, current := range monitor.hosts {
		input.Hosts = append(input.Hosts, current.view.Host)
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
		return nil, "", "", err
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
		result = append(result, HostRecord{Alias: "this host", ID: local.ID, MeshIdentity: local.ID})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Alias < result[j].Alias })
	return result, local.ID, filepath.Join(stateDir, "daemon.sock"), nil
}

func (m *dashboardMonitor) dialHost(ctx context.Context, host HostRecord) (transport.Conn, error) {
	if host.ID != m.localID {
		return m.dial(ctx, host)
	}
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", m.localSocket)
	if err != nil {
		return nil, err
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
	}
	return conn, err
}

func (m *dashboardMonitor) loadCache(ctx context.Context) {
	for _, current := range m.hosts {
		m.loadHostCache(ctx, current)
	}
}

func (m *dashboardMonitor) loadHostCache(ctx context.Context, current *dashboardHostState) {
	stored, err := m.cache.store.GetHost(ctx, storage.HostID(current.record.ID))
	if err != nil || stored.MeshIdentity != current.record.MeshIdentity {
		return
	}
	current.view.LastReply = stored.LastSeenAt
	rows, err := m.cache.Load(ctx, current.record)
	if err == nil {
		current.view.Sessions = dashboardSessions(rows, time.Time{})
		current.view.SessionsStale = true
	}
	services, err := m.cache.LoadServices(ctx, current.record)
	if err != nil {
		return
	}
	current.view.ServicesStale = true
	for _, row := range services {
		state := "ready"
		if !row.Healthy {
			state = "unhealthy"
		}
		current.view.Services = append(current.view.Services, DashboardService{Name: row.Service.Name, State: state, Problem: row.Problem})
		if current.view.ServicesObservedAt.IsZero() || row.ObservedAt.Before(current.view.ServicesObservedAt) {
			current.view.ServicesObservedAt = row.ObservedAt
		}
	}
}
