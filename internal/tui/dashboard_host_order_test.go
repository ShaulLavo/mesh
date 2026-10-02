package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardHostsOrderedByRAM(t *testing.T) {
	now := time.Unix(1700000000, 0)
	input := cli.DashboardInput{Wall: true}
	for _, alias := range []string{"vps", "unknown-z", "macbook-air", "pi", "unknown-a", "pc"} {
		input.Hosts = append(input.Hosts, cli.DashboardHost{ID: alias, Alias: alias})
	}
	model := newDashboard(input, now)
	t.Run("before metrics", func(t *testing.T) {
		assertDashboardHostOrder(t, model, []string{"macbook-air", "pc", "pi", "unknown-a", "unknown-z", "vps"})
	})
	for _, host := range input.Hosts {
		total := map[string]uint64{"pc": 32 << 30, "macbook-air": 16 << 30, "pi": 4 << 30, "vps": 4 << 30}[host.ID]
		model.receive(cli.DashboardHostView{Host: host, Connection: cli.StateReachable, LastReply: now,
			RAM:      cli.DashboardMeasurement[cli.DashboardMemory]{State: statusAvailable, Value: cli.DashboardMemory{TotalBytes: total, AvailableBytes: total / 2}, MeasuredAt: now},
			Sessions: cli.DashboardCatalog[cli.DashboardSession]{Total: 1, ObservedAt: now, Rows: []cli.DashboardSession{{ID: "12345", Command: "bash", State: dashboardRunning}}},
			Services: cli.DashboardCatalog[cli.DashboardService]{Total: 1, ObservedAt: now, Rows: []cli.DashboardService{{Name: host.Alias, State: "ready"}}},
		})
	}
	want := []string{"pc", "macbook-air", "pi", "vps", "unknown-a", "unknown-z"}
	assertDashboardHostOrder(t, model, want)
	for _, services := range []bool{false, true} {
		rows, _ := model.summaryRows(services, 120)
		for index, alias := range want {
			prefix := alias
			if services {
				prefix = strings.TrimSpace(dashboardFit(alias, 10))
			}
			if !strings.HasPrefix(ansi.Strip(rows[index].text), prefix) {
				t.Fatalf("services=%v row %d did not follow host order: %q", services, index, rows[index].text)
			}
		}
	}
	for _, host := range slices.Clone(model.hosts) {
		host.RAM.Value.AvailableBytes = 0
		host.CPU.Value = 100
		model.receive(host)
	}
	assertDashboardHostOrder(t, model, want)
	pc := model.hosts[0]
	pc.Connection = cli.StateUnreachable
	pc.RAM = cli.DashboardMeasurement[cli.DashboardMemory]{}
	model.receive(pc)
	assertDashboardHostOrder(t, model, want)
	pc.Connection = cli.StateReachable
	pc.RAM.Value.TotalBytes = 2 << 30
	model.receive(pc)
	assertDashboardHostOrder(t, model, []string{"macbook-air", "pi", "vps", "pc", "unknown-a", "unknown-z"})
}

func assertDashboardHostOrder(t *testing.T, model dashboardModel, want []string) {
	t.Helper()
	var got []string
	for _, host := range model.hosts {
		got = append(got, host.Host.Alias)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("host order = %v, want %v", got, want)
	}
}

func TestDashboardSessionHostColumnAtWallSize(t *testing.T) {
	model := dashboardHostColumnFixture()
	next, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	model = next.(dashboardModel)
	leftWidth := (model.width - 1) * 3 / 5
	panel := ansi.Strip(strings.Join(model.sessionSummary(leftWidth, 15), "\n"))
	var heading, row string
	for _, line := range strings.Split(panel, "\n") {
		if strings.Contains(line, "COMMAND (launch)") {
			heading = line
		}
		if strings.Contains(line, "12345") && strings.Contains(line, "macbook") {
			row = line
		}
	}
	if !strings.Contains(row, "macbook-air 12345") {
		t.Fatalf("11-cell host name was truncated despite spare command space: %q", row)
	}
	if strings.Index(heading, "ID")-strings.Index(heading, "HOST") != 12 || strings.Index(row, "12345") != strings.Index(heading, "ID") {
		t.Fatalf("host column or heading alignment is wrong:\n%s\n%s", heading, row)
	}
	assertFits(t, model.View().Content, 160, 45)
	fmt.Printf("\nBEGIN_HOST_ORDER_160_45\n%s\nEND_HOST_ORDER_160_45\n", model.View().Content)
}

func dashboardHostColumnFixture() dashboardModel {
	model := dashboardPerformanceFixture()
	model.hosts[1].Host.Alias = "macbook-air"
	model.hosts[1].Sessions.Rows[0].ID = "12345"
	model.hosts[3].RAM.Value.TotalBytes = model.hosts[2].RAM.Value.TotalBytes
	for _, host := range slices.Clone(model.hosts) {
		model.receive(host)
	}
	return model
}

func BenchmarkDashboardHostOrderRender(b *testing.B) {
	model := dashboardHostColumnFixture()
	b.ReportAllocs()
	for b.Loop() {
		_ = model.render()
	}
}

func TestDashboardSessionHostWidthCapsAndCachedAlignment(t *testing.T) {
	model := dashboardHostColumnFixture()
	for _, test := range []struct {
		alias string
		width int
		want  int
	}{
		{"macbook-air", 91, 11},
		{"macbook-air", 76, 11},
		{"a-very-long-machine-name", 91, 20},
		{"a-very-long-machine-name", 76, 19},
		{"日本語ホスト", 91, 12},
	} {
		model.hosts[1].Host.Alias = test.alias
		if got := model.sessionHostWidth(test.width); got != test.want {
			t.Errorf("alias %q at width %d: HOST width %d, want %d", test.alias, test.width, got, test.want)
		}
	}
	model.hosts[1].Host.Alias = "macbook-air"
	model.hosts[1].Connection = cli.StateUnreachable
	panel := ansi.Strip(strings.Join(model.sessionSummary(95, 25), "\n"))
	if !strings.Contains(panel, "cached · macbook-air") || !strings.Contains(panel, "macbook-air 12345") {
		t.Fatalf("cached host name did not share the live column width: %s", panel)
	}
	assertFits(t, panel, 95, 25)
}
