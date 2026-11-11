package cli

import (
	"time"

	"github.com/shaul/mesh/internal/update"
)

func DashboardHostUpdateRows(hosts []DashboardHostView, now time.Time) []DashboardUpdateRow {
	rows := make([]DashboardUpdateRow, 0, len(hosts))
	for _, host := range hosts {
		if host.HostUpdate == nil {
			continue
		}
		member := FleetUpdateMember{Host: update.Host{ID: host.Host.ID}, Build: &host.Build, Update: host.HostUpdate, LastReply: host.LastReply}
		member.State = dashboardFleetConnection(host.Connection)
		code := classifyFleetUpdate(&member, now)
		text := "Mesh " + fleetVersion(member) + "; " + member.State + "."
		if code != 0 {
			text += " " + fleetUpdateAttention(member) + " Run mesh update fleet --details."
		}
		rows = append(rows, DashboardUpdateRow{HostID: host.Host.ID, Text: text, Attention: code == 1})
	}
	return rows
}

func dashboardFleetConnection(state StateConnection) string {
	switch state {
	case StateUnreachable:
		return fleetOffline
	case StateRefused:
		return fleetRefused
	case StateConnecting, StateReachable:
		return ""
	default:
		return ""
	}
}
