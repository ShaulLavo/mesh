package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardLegacyMetricFeedExplainsUpgradeOnce(t *testing.T) {
	now := time.Unix(1700000000, 0)
	host := cli.DashboardHostView{Host: cli.DashboardHost{ID: "legacy", MachineName: "mac"}, Connection: cli.StateReachable, LastReply: now, MetricsUnsupported: true}
	host.CPU.State, host.RAM.State, host.Temperature.State, host.Uptime.State = "unsupported", "unsupported", "unsupported", "unsupported"
	host.Sessions.ObservedAt, host.Services.ObservedAt = now, now
	for _, size := range [][2]int{{160, 48}, {80, 24}} {
		model := newDashboard(cli.DashboardInput{}, now)
		model.width, model.height = size[0], size[1]
		model.hosts = []cli.DashboardHostView{host}
		text := ansi.Strip(model.render())
		if strings.Count(text, "needs mesh v0.1.114+") != 1 || strings.Contains(text, "unsupported") || strings.Contains(text, "metrics never") {
			t.Fatalf("legacy feed repeats unknown metrics or hides its upgrade explanation at%dx%d: %s", size[0], size[1], text)
		}
	}
}
