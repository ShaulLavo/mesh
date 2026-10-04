package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardHostBurstPublishesOneCompleteClockFrame(t *testing.T) {
	now := time.Unix(1700000000, 0)
	input := cli.DashboardInput{Hosts: []cli.DashboardHost{{ID: "host", MachineName: "pc"}}, Wall: true}
	model := newDashboard(input, now)
	next, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	model = next.(dashboardModel)
	previous := model.View().Content
	for count := range 3 {
		next, _ = model.Update(dashboardHostMsg(cli.DashboardHostView{Host: input.Hosts[0], Connection: cli.StateReachable, LastReply: now, Sessions: cli.DashboardCatalog[cli.DashboardSession]{Total: count + 1, ObservedAt: now}, Services: cli.DashboardCatalog[cli.DashboardService]{ObservedAt: now}}))
		model = next.(dashboardModel)
		if model.View().Content != previous {
			t.Fatal("host publication repainted between clock frames")
		}
	}
	next, _ = model.Update(dashboardTickMsg(now.Add(time.Second)))
	model = next.(dashboardModel)
	rendered := ansi.Strip(model.View().Content)
	if !strings.Contains(rendered, "sessions 3 live") {
		t.Fatalf("clock frame lost latest host facts: %s", rendered)
	}
	next, _ = model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = next.(dashboardModel)
	assertFits(t, model.View().Content, 80, 24)
}

func TestDashboardRetainedViewDoesNotAllocatePerCall(t *testing.T) {
	model := newDashboard(cli.DashboardInput{Wall: true}, time.Unix(1700000000, 0))
	next, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	model = next.(dashboardModel)
	if allocations := testing.AllocsPerRun(100, func() { _ = model.View() }); allocations != 0 {
		t.Fatalf("retained View rebuilt terminal content: %v allocations", allocations)
	}
}

func TestDashboardUnknownServicesStayOutOfAttention(t *testing.T) {
	now := time.Unix(1700000000, 0)
	model := newDashboard(cli.DashboardInput{}, now)
	model.hosts = []cli.DashboardHostView{{Host: cli.DashboardHost{MachineName: "pi"}, Connection: cli.StateReachable, LastReply: now, Services: cli.DashboardCatalog[cli.DashboardService]{ObservedAt: now, Total: 1, Unknown: 1, Rows: []cli.DashboardService{{Name: "legacy", State: "unknown", HealthUnknown: true}}}}}
	if totals := model.totals(); totals.ready != 0 || totals.failed != 0 || totals.unknown != 1 {
		t.Fatalf("unknown totals: %+v", totals)
	}
	attention := ansi.Strip(strings.Join(model.attention(80, 5), "\n"))
	if attention != "" {
		t.Fatalf("unknown health entered Attention: %s", attention)
	}
	rows := model.serviceRows(model.hosts[0], 80)
	if !strings.Contains(rows[0].text, model.paint(dashboardMutedStyle).Render("● unknown")) {
		t.Fatal("unknown health painted as ready")
	}
}
