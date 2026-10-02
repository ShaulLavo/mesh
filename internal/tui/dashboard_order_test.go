package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardCardsOrderByCapacity(t *testing.T) {
	model := dashboardPerformanceFixture()
	model.hosts[1].Host.Alias = "macbook-air"
	model.hosts[3].RAM.Value.TotalBytes = model.hosts[2].RAM.Value.TotalBytes
	// Discovery order deliberately differs from capacity and name order.
	model.hosts = []cli.DashboardHostView{model.hosts[3], model.hosts[2], model.hosts[1], model.hosts[0]}
	inputOrder := append([]cli.DashboardHostView(nil), model.hosts...)
	assertDashboardCardOrder(t, model, [][]string{{"pc", "macbook-air"}, {"pi", "vps"}})
	for index, host := range model.hosts {
		if host.Host.ID != inputOrder[index].Host.ID {
			t.Fatal("render mutated the retained host inventory")
		}
	}
	for index, host := range inputOrder {
		host.CPU.Value = float64(100 - index*30)
		host.RAM.Value.AvailableBytes = uint64(index) * host.RAM.Value.TotalBytes / 4
		host.RAM.MeasuredAt = model.now.Add(-time.Minute)
		host.RAM.Failing = true
		updated, _ := model.Update(dashboardHostMsg(host))
		model = updated.(dashboardModel)
		assertDashboardCardOrder(t, model, [][]string{{"pc", "macbook-air"}, {"pi", "vps"}})
	}
	updated, _ := model.Update(dashboardTickMsg(model.now.Add(time.Second)))
	assertDashboardCardOrder(t, updated.(dashboardModel), [][]string{{"pc", "macbook-air"}, {"pi", "vps"}})
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	assertDashboardCardOrder(t, updated.(dashboardModel), [][]string{{"pc", "macbook-air"}, {"pi", "vps"}})
}

func TestDashboardCardsUnmeasuredHostsLast(t *testing.T) {
	model := dashboardPerformanceFixture()
	model.hosts[1].Host.Alias = "macbook-air"
	model.hosts[3].RAM.Value = cli.DashboardMemory{}
	model.hosts[3].Host.Alias = "a-unmeasured"
	model.hosts[2].RAM.Value = cli.DashboardMemory{}
	model.hosts[2].Host.Alias = "z-unmeasured"
	model.hosts = []cli.DashboardHostView{model.hosts[2], model.hosts[3], model.hosts[1], model.hosts[0]}
	assertDashboardCardOrder(t, model, [][]string{{"pc", "macbook-air"}, {"a-unmeasured", "z-unmeasured"}})
	// The first capacity reading admits a previously unmeasured host at its fixed rank.
	host := model.hosts[0]
	host.RAM.Value.TotalBytes = 64 << 30
	updated, _ := model.Update(dashboardHostMsg(host))
	assertDashboardCardOrder(t, updated.(dashboardModel), [][]string{{"z-unmeasured", "pc"}, {"macbook-air", "a-unmeasured"}})
}

func assertDashboardCardOrder(t *testing.T, model dashboardModel, rows [][]string) {
	t.Helper()
	frame := model.render()
	assertFits(t, frame, 160, 45)
	var headings []string
	for _, line := range strings.Split(ansi.Strip(frame), "\n") {
		if strings.HasPrefix(line, "┌") && strings.Contains(line, "metrics ") {
			headings = append(headings, line)
		}
	}
	if len(headings) != len(rows) {
		t.Fatalf("card headings = %d, want %d: %s", len(headings), len(rows), ansi.Strip(frame))
	}
	for index, row := range rows {
		left, right := strings.Index(headings[index], row[0]), strings.Index(headings[index], row[1])
		if left < 0 || left >= 80 || right < 80 {
			t.Errorf("row %d wants %s left, %s right: %s", index, row[0], row[1], headings[index])
		}
	}
}

func TestDashboardSessionsHostColumnFitsNames(t *testing.T) {
	for _, alias := range []string{"macbook-air", "shauls-macbook-air", "開発-macbook-air"} {
		t.Run(alias, func(t *testing.T) {
			model := dashboardPerformanceFixture()
			model.hosts[1].Host.Alias = alias
			for _, width := range []int{91, 116} {
				lines := model.sessionSummary(width+4, 30)
				plain := ansi.Strip(strings.Join(lines, "\n"))
				if !strings.Contains(plain, alias+" ") {
					t.Errorf("width %d clipped host %q: %s", width, alias, plain)
				}
				assertFits(t, strings.Join(lines, "\n"), width+4, 30)
			}
			// Header and rows use one column width, including shorter host names.
			rows, _ := model.summaryRows(false, 91)
			var idColumn int
			for index, row := range rows {
				fields := strings.Fields(ansi.Strip(row.text))
				if len(fields) < 2 {
					t.Fatal("session row lost columns")
				}
				column := ansi.StringWidth(strings.Split(ansi.Strip(row.text), fields[1])[0])
				if index == 0 {
					idColumn = column
				}
				if column != idColumn {
					t.Fatal("session ID columns do not align")
				}
			}
			model.width, model.height = 80, 24
			assertFits(t, model.render(), 80, 24)
		})
	}
}

func TestDashboardCapacityOrder160x45Evidence(t *testing.T) {
	model := dashboardPerformanceFixture()
	model.hosts[1].Host.Alias = "macbook-air"
	model.hosts[3].RAM.Value.TotalBytes = model.hosts[2].RAM.Value.TotalBytes
	frame := model.render()
	assertFits(t, frame, 160, 45)
	fmt.Printf("\nBEGIN_ORDER_160_45\n%s\nEND_ORDER_160_45\n", frame)
}
