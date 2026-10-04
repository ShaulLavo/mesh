package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/hostmetrics"
)

func TestDashboardFirstMetricsAdvice(t *testing.T) {
	for _, size := range [][2]int{{160, 45}, {80, 24}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			model := dashboardPerformanceFixture()
			model.width, model.height = size[0], size[1]
			model.hosts = model.hosts[:1]
			host := &model.hosts[0]
			host.PerformanceVersion = 0
			host.CPU = cli.DashboardMeasurement[float64]{State: cli.DashboardMeasurementPending}
			host.RAM = cli.DashboardMeasurement[cli.DashboardMemory]{State: cli.DashboardMeasurementPending}
			host.Uptime = cli.DashboardMeasurement[uint64]{State: cli.DashboardMeasurementPending}
			host.Temperature = cli.DashboardMeasurement[cli.DashboardTemperature]{State: cli.DashboardMeasurementPending}
			host.GPU, host.Battery, host.Disk, host.Network, host.Cores = nil, nil, nil, nil, nil
			host.Temperatures = nil
			view := ansi.Strip(model.render())
			assertFits(t, model.render(), size[0], size[1])
			if strings.Count(view, "Waiting for metrics…") != 1 || !strings.Contains(view, "pending") || strings.Contains(view, dashboardPerformanceUpgrade) {
				t.Fatalf("first frame needs one waiting note with pending readings: %s", view)
			}
			host.CPU.Failing, host.RAM.Failing = true, true
			view = ansi.Strip(model.render())
			if !strings.Contains(view, "Metrics unavailable") || strings.Contains(view, "Waiting for metrics") || strings.Contains(view, dashboardPerformanceUpgrade) || strings.Contains(view, "pending") {
				t.Fatalf("first metrics error presented as healthy startup or older producer: %s", view)
			}
			host.Problem = "cli: daemon rejected state watch: permission denied"
			host.NameFailing = true
			view = ansi.Strip(model.render())
			if !strings.Contains(view, "permission denied") || strings.Contains(view, "daemon rejected state watch") || strings.Contains(view, dashboardPerformanceUpgrade) || strings.Contains(view, "Waiting for metrics") {
				t.Fatalf("first metrics error detail hidden: %s", view)
			}
			host.Problem = ""
			host.NameFailing = false
			host.PerformanceVersion = 1
			host.CPU.State, host.RAM.State = hostmetrics.Unavailable, hostmetrics.Unavailable
			view = ansi.Strip(model.render())
			if strings.Contains(view, "Waiting for metrics") || strings.Contains(view, dashboardPerformanceUpgrade) || !strings.Contains(view, "unavailable") {
				t.Fatalf("received unavailable metrics confused with waiting or legacy: %s", view)
			}
		})
	}
}
