package tui

import (
	"bytes"
	tea "charm.land/bubbletea/v2"
	"context"
	"fmt"
	"github.com/charmbracelet/colorprofile"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardDesignRegionsAndOfflineFacts(t *testing.T) {
	model := dashboardDesignFixture(4)
	view := ansi.Strip(model.render())
	assertFits(t, model.render(), 160, 48)
	for _, text := range []string{"reachable 3", "unreachable 1", "sessions 7 live 2 cached", "ready 4", "failed 1", "ACTIVITY", "SESSION", "AGE", "last verified reply 32m", "cached catalog 32m", "Attention", "metrics 2s", "catalogs change-driven", "0–100%"} {
		if !strings.Contains(view, text) {
			t.Fatalf("missing design region %q: %s", text, view)
		}
	}
	card := ansi.Strip(strings.Join(model.card(dashboardFixtureHost(model, "macbook"), 79), "\n"))
	if strings.Contains(card, "CPU") || strings.Contains(card, "RAM") || !strings.Contains(card, "sessions 2 cached") {
		t.Fatalf("offline reading presented as current: %s", card)
	}
	for _, size := range [][2]int{{140, 40}, {80, 24}, {50, 12}} {
		model.width, model.height = size[0], size[1]
		assertFits(t, model.render(), size[0], size[1])
	}
}

func TestDashboardDesignVariedFilledHistoryAndConsoleFallback(t *testing.T) {
	model := dashboardDesignFixture(4)
	panel := strings.Join(model.area(model.history["pc"].cpu, model.now, 37, 4, false, dashboardCPUStyle), "\n")
	if strings.Count(panel, "█") < 15 || !strings.ContainsAny(panel, "⠉⠒⠤⣀") {
		t.Fatalf("history has no multi-row filled variation: %s", panel)
	}
	model.ascii = true
	view := model.render()
	if strings.ContainsAny(ansi.Strip(view), "┌┐└┘▁▂▃▄▅▆▇█▪●⠉⠒⠤⣀") {
		t.Fatal("console view retained Unicode plot/border marks")
	}
	assertFits(t, view, 160, 48)
}

func TestDashboardDesignEvidence(t *testing.T) {
	for _, count := range []int{4, 6} {
		model := dashboardDesignFixture(count)
		view := model.render()
		assertFits(t, view, 160, 48)
		fmt.Printf("\nBEGIN_DESIGN_%d_160\n%s\nEND_DESIGN_%d_160\n", count, view, count)
	}
	model := dashboardDesignFixture(4)
	model.width, model.height = 80, 24
	assertFits(t, model.render(), 80, 24)
	fmt.Printf("\nBEGIN_DESIGN_4_80\n%s\nEND_DESIGN_4_80\n", model.render())
	model.width, model.height = 160, 48
	model.ascii = true
	fmt.Printf("\nBEGIN_DESIGN_4_LINUX\n%s\nEND_DESIGN_4_LINUX\n", model.render())
}

func dashboardDesignFixture(count int) dashboardModel {
	now := time.Date(2026, 10, 2, 14, 32, 7, 0, time.UTC)
	model := newDashboard(cli.DashboardInput{Wall: true}, now)
	model.width, model.height = 160, 48
	aliases := []string{"pc", "pi", "vps", "macbook", "build", "edge"}
	commands := []string{"codex", "bun run dev", "journalctl -fu mesh", "nvim draft.md", "go test ./...", "bash"}
	for i := range count {
		machineName := aliases[i]
		cpu := []float64{34, 3, 6, 12, 42, 12}[i]
		totalGiB := []uint64{64, 4, 4, 16, 32, 8}[i]
		usedGiB := []float64{18.2, 0.7, 1.1, 8, 8, 1.8}[i]
		metricsAt := now.Add(-time.Second)
		catalogAt := now.Add(-3 * time.Second)
		if i == 1 {
			catalogAt = now.Add(-time.Second)
		}
		if i == 2 {
			metricsAt = now.Add(-2 * time.Second)
			catalogAt = now.Add(-4 * time.Second)
		}
		host := cli.DashboardHostView{Host: cli.DashboardHost{ID: machineName, MachineName: machineName, Local: i == 1}, Connection: cli.StateReachable, LastReply: now,
			CPU:         cli.DashboardMeasurement[float64]{State: statusAvailable, Value: cpu, Sample: "instance/60", Segment: 1, MeasuredAt: metricsAt},
			RAM:         cli.DashboardMeasurement[cli.DashboardMemory]{State: statusAvailable, Value: cli.DashboardMemory{TotalBytes: totalGiB << 30, AvailableBytes: (totalGiB << 30) - uint64(math.Ceil(usedGiB*(1<<30))), Estimate: "Linux MemAvailable estimate"}, Sample: "instance/60", Segment: 1, MeasuredAt: metricsAt},
			Temperature: cli.DashboardMeasurement[cli.DashboardTemperature]{State: statusAvailable, Value: cli.DashboardTemperature{Sensor: "cpu", Celsius: []float64{58, 44, 0, 40, 61, 38}[i]}, Sample: "temp/1", MeasuredAt: now},
			Uptime:      cli.DashboardMeasurement[uint64]{State: statusAvailable, Value: []uint64{3*86400 + 7*3600, 12*86400 + 4*3600, 26*86400 + 3600, 86400, 86400 + 5*3600, 3 * 3600}[i], Sample: "up/1", MeasuredAt: now},
			Sessions:    cli.DashboardCatalog[cli.DashboardSession]{ObservedAt: catalogAt},
			Services:    cli.DashboardCatalog[cli.DashboardService]{ObservedAt: catalogAt},
		}
		sessions := 1
		switch i {
		case 0:
			sessions = 4
		case 2, 3:
			sessions = 2
		}
		for n := range sessions {
			name, command := []string{"codex audit", "maintenance", "edge", "writing", "build", "shell"}[i], commands[i]
			state := "detached"
			if i == 1 || i == 0 && n == 1 {
				state = "running"
			}
			if i == 0 && n == 1 {
				name, command = "bun dev", "bun run dev"
			}
			if i == 0 && n > 1 {
				name, command = "review", "bash"
			}
			if i == 1 {
				command = "journalctl -fu mesh"
			}
			if i == 2 && n == 1 {
				name, command = "bash", "bash"
			}
			if i == 3 && n == 1 {
				name, command = "scratch", "zsh"
			}
			host.Sessions.Rows = append(host.Sessions.Rows, cli.DashboardSession{ID: fmt.Sprintf("%dK%dD", i, n), Name: name, State: state, Command: command})
		}
		host.Sessions.Total = sessions
		if i == 2 {
			host.Temperature.State = "unsupported"
		}
		if i == 0 {
			host.Services.Rows = []cli.DashboardService{{Name: "api", State: "ready"}, {Name: "preview", State: "failed", Problem: "process exited", Failed: true}}
		}
		if i == 2 {
			host.Services.Rows = []cli.DashboardService{{Name: "edge", State: "ready"}, {Name: "blog", State: "ready"}, {Name: "notes", State: "ready"}}
		}
		host.Services.Total = len(host.Services.Rows)
		for _, service := range host.Services.Rows {
			if service.Failed {
				host.Services.Failed++
			} else {
				host.Services.Ready++
			}
		}
		if i == 3 {
			host.CPU.MeasuredAt = now.Add(-32 * time.Minute)
			host.RAM.MeasuredAt = now.Add(-32 * time.Minute)
			host.Connection = cli.StateUnreachable
			host.LastReply = now.Add(-32 * time.Minute)
			host.Sessions.ObservedAt = host.LastReply
			host.Services.ObservedAt = host.LastReply
			host.Sessions.Failing = true
			host.Services.Failing = true
			host.Problem = "connection timed out"
		}
		model.hosts = append(model.hosts, cli.DashboardHostView{Host: host.Host})
		model.receive(host)
		if i == 3 {
			continue
		}
		history := dashboardHostHistory{}
		for n := range 60 {
			at := metricsAt.Add(time.Duration(n-59) * 2 * time.Second)
			value := float64([]int{22, 24, 28, 26, 34, 42, 36, 30, 26, 30, 46, 58, 44, 34, 28, 26, 32, 40, 34, 30, 26, 28, 36, 42, 36, 32}[n%26])
			if i == 1 {
				value = float64(1 + n%4)
			}
			if i == 2 {
				value = float64([]int{5, 4, 3, 4, 6, 7, 6, 5, 4, 6, 8, 6, 4, 5, 3, 4}[n%16])
			}
			if n == 59 {
				value = cpu
			}
			history.cpu = append(history.cpu, dashboardPoint{at: at, sample: fmt.Sprint(n), segment: 1, value: value})
			history.ram = append(history.ram, dashboardPoint{at: at, sample: fmt.Sprint(n), segment: 1, value: dashboardMemoryPercent(host.RAM.Value) - float64(59-n)/180})
		}
		model.history[machineName] = history
	}
	return model
}

func TestDashboardAreaFixedPercentHeight(t *testing.T) {
	now := time.Unix(1700000000, 0)
	model := newDashboard(cli.DashboardInput{}, now)
	// 100% is the positive control: all four rows must be filled.
	for _, test := range []struct {
		value float64
		units int
	}{{100, 32}, {0, 0}, {25, 8}, {28, 9}, {50, 16}} {
		points := []dashboardPoint{{at: now, value: test.value}}
		lines := model.area(points, now, 1, 4, false, dashboardRAMStyle)
		units := 0
		for _, line := range lines {
			for _, cell := range ansi.Strip(line) {
				switch cell {
				case '⠉':
					units += 8
				case '⠒':
					units += 6
				case '⠤':
					units += 4
				case '⣀':
					units += 2
				default:
					if index := strings.IndexRune(" ▁▂▃▄▅▆▇█", cell); index >= 0 {
						units += (index + 2) / 3
					}
				}
			}
		}
		if units < test.units || units > test.units+1 {
			t.Fatalf("%.0f%% rendered %d/32 height units, want %d: %q", test.value, units, test.units, lines)
		}
	}
}

func TestDashboardDesignDenseMeterAndHumanDurations(t *testing.T) {
	now := time.Unix(1700000000, 0)
	model := newDashboard(cli.DashboardInput{}, now)
	metric := cli.DashboardMeasurement[float64]{State: statusAvailable, Value: 100, Sample: "i/1", MeasuredAt: now}
	meter := ansi.Strip(model.segmentedMeter(metric, now, true, 37, false, dashboardCPUStyle))
	if strings.Count(meter, "▪") != 37 {
		t.Fatalf("meter skips cells instead of thin full-width segments: %q", meter)
	}
	host := cli.DashboardHostView{Connection: cli.StateReachable, LastReply: now, Uptime: cli.DashboardMeasurement[uint64]{State: statusAvailable, Value: 3*86400 + 7*3600, Sample: "i/1", MeasuredAt: now}}
	if got := model.uptime(host); got != "3d 7h" {
		t.Fatalf("uptime %q", got)
	}
	if got := dashboardAge(now, now.Add(-32*time.Minute)); got != "32m" {
		t.Fatalf("catalog age %q", got)
	}
}

func TestDashboardDesignContentSizedGroupedTables(t *testing.T) {
	model := dashboardDesignFixture(4)
	sessions := strings.Join(model.summary(false, 95, 20), "\n")
	if !strings.Contains(ansi.Strip(sessions), "cached · macbook · catalog 32m old · not counted as live") {
		t.Fatalf("missing separate cached catalog heading: %s", sessions)
	}
	services := model.summary(true, 64, 20)
	if len(services) > 9 {
		t.Fatalf("five services padded to %d rows", len(services))
	}
}

func TestDashboardAreaUsesThinEdgeOverDimFill(t *testing.T) {
	now := time.Unix(1700000000, 0)
	model := newDashboard(cli.DashboardInput{}, now)
	points := []dashboardPoint{{at: now, value: 28}}
	area := strings.Join(model.area(points, now, 1, 4, false, dashboardRAMStyle), "\n")
	if !strings.ContainsAny(ansi.Strip(area), "⠉⠒⠤⣀") {
		t.Fatalf("fractional area paints a thick bright block instead of a thin edge: %q", area)
	}
	if !strings.Contains(area, "38;2;6;42;54") {
		t.Fatalf("area has no dim RAM fill: %q", area)
	}
}

func TestDashboardDesignConsoleEvidence(t *testing.T) {
	model := dashboardDesignFixture(4)
	model.ascii, model.wall = true, false
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var output bytes.Buffer
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(&output), tea.WithWindowSize(160, 48), tea.WithoutSignals(), tea.WithColorProfile(colorprofile.ANSI))
	go func() { time.Sleep(100 * time.Millisecond); program.Send(dashboardDoneMsg{}) }()
	if _, err := program.Run(); err != nil {
		t.Fatal(err)
	}
	raw := output.String()
	if strings.Contains(raw, "38;2;") || strings.Contains(raw, "38;5;") || strings.Contains(raw, "48;2;") || strings.Contains(raw, "48;5;") {
		t.Fatal("forced16-color console retained extendedcolors")
	}
	if !strings.Contains(ansi.Strip(raw), "CPU ") {
		t.Fatal("console positivecontrol has no hostcard")
	}
	fmt.Printf("\nBEGIN_DESIGN_4_LINUX16\n%s\nEND_DESIGN_4_LINUX16\n", raw)
}

func TestDashboardCompactKeepsLiveRowsBeforeCachedGroups(t *testing.T) {
	model := dashboardDesignFixture(4)
	model.width, model.height = 80, 24
	view := ansi.Strip(model.render())
	if !strings.Contains(view, "live 2/7") || !strings.Contains(view, "1K0D") || !strings.Contains(view, "0/2 cached") {
		t.Fatalf("cached groups displaced live rows in compact budget: %s", view)
	}
	assertFits(t, model.render(), 80, 24)
}

func dashboardFixtureHost(model dashboardModel, machineName string) cli.DashboardHostView {
	return model.hosts[slices.IndexFunc(model.hosts, func(host cli.DashboardHostView) bool { return host.Host.MachineName == machineName })]
}
