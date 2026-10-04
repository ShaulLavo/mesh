package tui

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardSummaryLineCountMatchesRenderedContent(t *testing.T) {
	random := rand.New(rand.NewPCG(13, 29)) //nolint:gosec // Deterministic fixture variations, not security randomness.
	for example := range 120 {
		model := dashboardTickFixture(t, example%2 == 0, 4)
		model.width = []int{140, 160, 210}[example%3]
		model.height = []int{40, 45, 48, 60}[example%4]
		model.now = model.now.Add(time.Duration(example%37) * time.Second)
		model.hosts = model.hosts[:random.IntN(5)]
		for index := range model.hosts {
			host := &model.hosts[index]
			host.Connection = []cli.StateConnection{cli.StateReachable, cli.StateUnreachable, cli.StateConnecting}[random.IntN(3)]
			host.Sessions.Total = random.IntN(12)
			host.Sessions.Rows = make([]cli.DashboardSession, random.IntN(host.Sessions.Total+1))
			for row := range host.Sessions.Rows {
				host.Sessions.Rows[row] = cli.DashboardSession{ID: "12345", Name: "session", State: "running", Command: "bash"}
			}
			host.Services.Total = random.IntN(14)
			host.Services.Rows = make([]cli.DashboardService, random.IntN(host.Services.Total+1))
			host.Services.Failed = 0
			for row := range host.Services.Rows {
				failed := random.IntN(3) == 0
				host.Services.Rows[row] = cli.DashboardService{Name: "service", State: "ready", Failed: failed, Problem: strings.Repeat("failure details ", random.IntN(8))}
				if failed {
					host.Services.Failed++
				}
			}
		}
		if got, want := model.summariesHeight(model.height), len(model.summaries(model.height)); got != want {
			t.Fatalf("example %d usage=%t size=%dx%d hosts=%d: measured %d rows, rendered %d", example, model.usageEnabled, model.width, model.height, len(model.hosts), got, want)
		}
	}
}

func dashboardCheckCachedFrame(t *testing.T, model dashboardModel) dashboardModel {
	t.Helper()
	next, _ := model.Update(dashboardTickMsg(model.now))
	updated := next.(dashboardModel)
	fresh := model
	fresh.layout = dashboardLayout{}
	if got, want := updated.View().Content, fresh.render(); got != want {
		t.Fatal("cached geometry differs from a fresh render")
	}
	return updated
}

func TestDashboardGeometryCacheTracksLayoutInputs(t *testing.T) {
	model := dashboardTickFixture(t, true, 4)
	work := dashboardRenderWork{}
	model.renderWork = &work
	model = dashboardCheckCachedFrame(t, model)
	first := model.layout
	work = dashboardRenderWork{}
	model.now = model.now.Add(time.Second)
	next, _ := model.Update(dashboardTickMsg(model.now))
	model = next.(dashboardModel)
	if work.layouts != 0 || model.layout != first {
		t.Fatalf("a clock-only tick rebuilt stable geometry: %+v", work)
	}
	model.hosts[0].CPU.Value = 89
	work = dashboardRenderWork{}
	next, _ = model.Update(dashboardTickMsg(model.now))
	model = next.(dashboardModel)
	if work.layouts != 0 {
		t.Fatal("a metric value rebuilt stable geometry")
	}
	for _, change := range []struct {
		name  string
		apply func(*dashboardModel)
	}{
		{"usage accounts", func(m *dashboardModel) { m.usage.accounts = nil; m.usage.total = 0 }},
		{"catalog rows", func(m *dashboardModel) { m.hosts[0].Services.Rows = nil; m.hosts[0].Services.Total = 0 }},
		{"wrapped attention", func(m *dashboardModel) {
			m.hosts[1].Connection = cli.StateUnreachable
			m.hosts[0].Services.Rows = []cli.DashboardService{{Name: "broken", State: "failed", Failed: true, Problem: strings.Repeat("health check details ", 20)}}
		}},
		{"GPU pairing", func(m *dashboardModel) { m.hosts[0].GPU = nil; m.hosts[1].GPU = nil }},
		{"host count", func(m *dashboardModel) { m.hosts = m.hosts[:3] }},
		{"resize", func(m *dashboardModel) { m.width, m.height = 140, 40 }},
		{"catalog freshness", func(m *dashboardModel) { m.now = m.now.Add(31 * time.Second) }},
		{"narrow resize", func(m *dashboardModel) { m.width, m.height = 80, 24 }},
		{"profile", func(m *dashboardModel) { m.profile, m.ascii = colorprofile.ANSI, true }},
		{"theme", func(m *dashboardModel) { m.palette = dashboardTheme("kanagawa") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			change.apply(&model)
			model = dashboardCheckCachedFrame(t, model)
		})
	}
}

func TestDashboardGeometryCacheAcrossRetainedModelMessages(t *testing.T) {
	model := dashboardTickFixture(t, true, 4)
	model = dashboardCheckCachedFrame(t, model)
	previous := model.View().Content
	host := model.hosts[1]
	host.Host.MachineName = "changed machineName"
	host.Services.Rows = []cli.DashboardService{{Name: "new", State: "ready"}}
	host.Services.Total = 1
	next, _ := model.Update(dashboardHostMsg(host))
	model = next.(dashboardModel)
	if model.View().Content != previous {
		t.Fatal("host message changed the retained clock frame")
	}
	model = dashboardCheckCachedFrame(t, model)
	for _, size := range [][2]int{{160, 48}, {140, 40}, {80, 24}, {50, 12}, {160, 45}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			next, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			model = next.(dashboardModel)
			model = dashboardCheckCachedFrame(t, model)
		})
	}
}

func TestDashboardCompactFramesSkipGridLayout(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {110, 32}, {110, 45}, {160, 39}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			model := dashboardTickFixture(t, true, 32)
			model.width, model.height = size[0], size[1]
			work := dashboardRenderWork{}
			model.renderWork = &work
			for range 2 {
				next, _ := model.Update(dashboardTickMsg(model.now))
				model = next.(dashboardModel)
			}
			if work.layouts != 0 || work.gpuPairs != 0 {
				t.Fatalf("compact frames prepared unused grid geometry: %+v", work)
			}
		})
	}
}
