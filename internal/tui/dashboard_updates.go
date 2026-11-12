package tui

import (
	"context"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/shaul/mesh/internal/cli"
)

type dashboardUpdatesMsg cli.DashboardUpdates

func watchDashboardUpdates(ctx context.Context, input cli.DashboardInput, program *tea.Program) {
	err := input.UpdateWatch(ctx, func(view cli.DashboardUpdates) {
		if ctx.Err() == nil {
			program.Send(dashboardUpdatesMsg(view))
		}
	})
	if err != nil && ctx.Err() == nil {
		program.Send(dashboardDoneMsg{err: err})
	}
}
func (m *dashboardModel) applyUpdates(view cli.DashboardUpdates) {
	if view.Generation <= m.updates.Generation {
		return
	}
	m.updates = view
}
func (m dashboardModel) updateLines() []string {
	if m.updateFrame != nil {
		return m.updateFrame
	}
	rows := cli.DashboardHostUpdateRows(m.hosts, m.now)
	rows = append(rows, cli.DashboardUpdateRows(m.updates, m.now)...)
	if len(rows) == 0 {
		return nil
	}
	slices.SortStableFunc(rows, func(a, b cli.DashboardUpdateRow) int {
		return dashboardUpdatePriority(b) - dashboardUpdatePriority(a)
	})
	limit := min(len(rows), 3)
	lines := make([]string, 0, limit+1)
	for _, row := range rows[:limit] {
		label := m.updateHostLabel(row.HostID)
		text := "Updates · " + label + safeText(row.Text)
		style := dashboardMutedStyle
		if row.Attention {
			style = dashboardCachedStyle
		}
		lines = append(lines, m.paint(style).Render(dashboardFit(text, m.width)))
	}
	if len(rows) > limit {
		lines = append(lines, m.paint(dashboardMutedStyle).Render(dashboardFit("Updates · more status in mesh update fleet; use --details to inspect.", m.width)))
	}
	return lines
}

func dashboardUpdatePriority(row cli.DashboardUpdateRow) int {
	if row.Attention {
		return 2
	}
	if row.Pending {
		return 1
	}
	return 0
}
func (m dashboardModel) updateHostLabel(id string) string {
	for _, host := range m.hosts {
		if host.Host.ID != id {
			continue
		}
		if host.Host.MachineName == "" {
			return "Selected machine: "
		}
		return safeText(m.privacy.Value("host", host.Host.MachineName)) + ": "
	}
	if id != "" {
		return "Selected machine: "
	}
	return ""
}
