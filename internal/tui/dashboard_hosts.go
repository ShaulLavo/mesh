package tui

import (
	"cmp"
	"slices"
	"strings"

	"github.com/shaul/mesh/internal/cli"
)

func (m *dashboardModel) rememberRAMTotal(host cli.DashboardHostView) {
	total := host.RAM.Value.TotalBytes
	// Keep a measured host's slot when a connection update has no RAM reading.
	if total == 0 || total == m.ramTotals[host.Host.ID] {
		return
	}
	m.ramTotals[host.Host.ID] = total
	m.sortHosts()
}

func (m *dashboardModel) sortHosts() {
	slices.SortFunc(m.hosts, func(a, b cli.DashboardHostView) int {
		if order := cmp.Compare(m.ramTotals[b.Host.ID], m.ramTotals[a.Host.ID]); order != 0 {
			return order
		}
		return strings.Compare(a.Host.Alias, b.Host.Alias)
	})
}
