package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardUnsupportedRetainedReading(t *testing.T) {
	now := time.Unix(1700000000, 0)
	host := cli.DashboardHostView{Connection: cli.StateReachable, LastReply: now}
	reading := cli.DashboardMeasurement[float64]{State: "unsupported"}
	if got := dashboardReading(reading, host, now, 10*time.Second, ""); got != "unsupported" {
		t.Fatalf("known-good initial unsupported reading: %q", got)
	}
	reading.Value, reading.Sample, reading.MeasuredAt = 25, "previous-daemon/1", now.Add(-2*time.Second)
	got := dashboardReading(reading, host, now, 10*time.Second, "25%")
	if !strings.Contains(got, "unsupported") {
		t.Fatalf("explicit unsupported capability with retained last-good data is presented only as %q", got)
	}
}

func TestDashboardHistoryCloseResetBoundary(t *testing.T) {
	now := time.Unix(1700000000, 0)
	old := cli.DashboardMeasurement[float64]{State: "available", Value: 50, Sample: "instance/1", Segment: 1, MeasuredAt: now.Add(-2 * time.Second)}
	points := dashboardRemember(nil, old, old.MeasuredAt)
	next := old
	next.Sample, next.Value, next.MeasuredAt = "instance/2", 10, now
	continuous := dashboardRemember(points, next, now)
	if value, found := dashboardGraphValue(continuous, now.Add(-time.Second)); !found || value != 50 {
		t.Fatal("known-good same-segment cadence should retain the previous observation")
	}
	next.Segment = 2
	reset := dashboardRemember(points, next, now)
	if len(reset) != 2 {
		t.Fatal("reset should retain the previous segment and the new sample")
	}
	if value, found := dashboardGraphValue(reset, now.Add(-time.Second)); found {
		t.Fatalf("graph paints %.0f%% through a counter-reset segment boundary", value)
	}
	if value, found := dashboardGraphValue(reset, old.MeasuredAt); !found || value != 50 {
		t.Fatal("earlier reset segment disappeared")
	}
	next.Segment = old.Segment
	next.Sample = "new-instance/1"
	restarted := dashboardRemember(points, next, now)
	if _, found := dashboardGraphValue(restarted, now.Add(-time.Second)); found {
		t.Fatal("sampler restart bridged history")
	}
}

func TestDashboardPanelUsesFourCorners(t *testing.T) {
	model := newDashboard(cli.DashboardInput{}, pickerTestNow)
	host := cli.DashboardHostView{Host: cli.DashboardHost{MachineName: "pc"}, Connection: cli.StateReachable}
	for _, width := range []int{80, 160} {
		panel := model.card(host, width)
		for i := range panel {
			panel[i] = ansi.Strip(panel[i])
		}
		if !strings.HasPrefix(panel[0], "┌") || !strings.HasSuffix(panel[0], "┐") || !strings.HasPrefix(panel[len(panel)-1], "└") || !strings.HasSuffix(panel[len(panel)-1], "┘") {
			t.Fatalf("incorrect panel corners at%d: %q / %q", width, panel[0], panel[len(panel)-1])
		}
		assertFits(t, strings.Join(panel, "\n"), width, 12)
	}
	model.ascii = true
	panel := model.card(host, 80)
	for i := range panel {
		panel[i] = ansi.Strip(panel[i])
	}
	if !strings.HasPrefix(panel[0], "+") || !strings.HasSuffix(panel[0], "+") || !strings.HasPrefix(panel[len(panel)-1], "+") || !strings.HasSuffix(panel[len(panel)-1], "+") {
		t.Fatal("ASCII corners lost")
	}
}

func TestDashboardLocalTitleNamesHostOnce(t *testing.T) {
	model := newDashboard(cli.DashboardInput{}, pickerTestNow)
	for _, machineName := range []string{"pi", "adopted-pc"} {
		host := cli.DashboardHostView{Host: cli.DashboardHost{MachineName: machineName, Local: true}, Connection: cli.StateReachable}
		for _, width := range []int{80, 160} {
			model.width, model.height = width, 48
			model.hosts = []cli.DashboardHostView{host}
			view := model.render()
			if strings.Count(ansi.Strip(model.hostTitle(host)), machineName) != 1 || strings.Contains(ansi.Strip(model.compactHost(host)[0]), "this host") {
				t.Fatalf("duplicated local name at%d: %s", width, view)
			}
		}
	}
}
