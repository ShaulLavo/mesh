package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/usagefeed"
)

func usageFixture(t testing.TB, name string) dashboardModel {
	t.Helper()
	data, err := os.ReadFile("testdata/usage/" + name + ".json") //nolint:gosec // name selects a checked-in fixture from the tests' fixed list
	if err != nil {
		t.Fatal(err)
	}
	var snapshot usagefeed.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	model := dashboardUsageFleetFixture()
	model.usageEnabled = true
	model.usage = projectDashboardUsage(&snapshot)
	return model
}

func dashboardUsageFleetFixture() dashboardModel {
	model := dashboardPerformanceFixture()
	previous := model.now
	model.now = time.Date(2026, 10, 2, 18, 53, 7, 0, time.UTC)
	model.width, model.height = 160, 45
	model.palette = dashboardTheme("oled")
	for index := range model.hosts {
		host := &model.hosts[index]
		host.Host.NameVerified = true
		host.NameObservedAt = model.now
		host.Host.Local = host.Host.ID == "pi"
		host.LastReply, host.CPU.MeasuredAt, host.RAM.MeasuredAt = model.now, model.now.Add(-time.Second), model.now.Add(-time.Second)
		host.Uptime.MeasuredAt = model.now
		if host.GPU != nil {
			host.GPU.MeasuredAt = model.now
		}
		if host.Cores != nil {
			host.Cores.MeasuredAt = model.now
		}
		if host.Battery != nil {
			host.Battery.MeasuredAt = model.now
		}
		if host.Disk != nil {
			host.Disk.MeasuredAt = model.now
		}
		if host.Network != nil {
			host.Network.MeasuredAt = model.now
		}
		for i := range host.Temperatures {
			host.Temperatures[i].MeasuredAt = model.now
		}
		host.Sessions = cli.DashboardCatalog[cli.DashboardSession]{ObservedAt: model.now.Add(-time.Second)}
		host.Services = cli.DashboardCatalog[cli.DashboardService]{ObservedAt: model.now.Add(-time.Second)}
		if host.Host.ID == "pc" {
			host.Sessions.Rows = []cli.DashboardSession{{ID: "E8WS", Name: "serve /work", State: "running", Command: "/usr/bin/bash -lc mesh serve"}}
			host.Sessions.Total = 1
			for n, name := range []string{"5173", "ai", "comfy", "life", "place", "platform", "workbench"} {
				state := "ready"
				if n == 0 {
					state = "idle"
				}
				if n == 1 {
					state = "running"
				}
				host.Services.Rows = append(host.Services.Rows, cli.DashboardService{Name: name, State: state})
			}
			host.Services.Total, host.Services.Ready, host.Services.Idle = 7, 6, 1
		}
		if host.Host.ID == "macbook" {
			host.Host.MachineName = "macbook-air"
			host.Sessions.Rows = []cli.DashboardSession{{ID: "N8PF", State: "detached", Command: "/bin/zsh"}}
			host.Sessions.Total = 1
		}
		history := model.history[host.Host.ID]
		for i := range history.cpu {
			history.cpu[i].at = history.cpu[i].at.Add(model.now.Sub(previous))
		}
		for i := range history.ram {
			history.ram[i].at = history.ram[i].at.Add(model.now.Sub(previous))
		}
		model.history[host.Host.ID] = history
	}
	for _, host := range slices.Clone(model.hosts) {
		model.receive(host)
	}
	return model
}

func TestDashboardUsageApprovedGrid(t *testing.T) {
	for _, name := range []string{"normal", "no-data", "overflow", "mixed", "historic", "model-scoped"} {
		t.Run(name, func(t *testing.T) {
			model := usageFixture(t, name)
			want, err := os.ReadFile("testdata/usage/" + name + ".txt") //nolint:gosec // name comes from the fixed fixture list above
			if err != nil {
				t.Fatal(err)
			}
			got := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n")) + "\n"
			if got != string(want) {
				t.Fatalf("approved 54×17 usage cells differ\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestDashboardUsageWindowFacts(t *testing.T) {
	model := usageFixture(t, "normal")
	window := model.usage.accounts[0].Windows[0]
	fresh := ansi.Strip(strings.Join(model.usageWindowLines(window, 50, true), "\n"))
	if !strings.Contains(fresh, "│") || !strings.Contains(fresh, "20% used · 80% left") {
		t.Fatal(fresh)
	}
	old := model.now.Add(-15 * time.Minute)
	window.LastSeenAt = &old
	if got := ansi.Strip(strings.Join(model.usageWindowLines(window, 50, true), "\n")); strings.Contains(got, "│") || !strings.Contains(got, "stale 15m") {
		t.Fatal(got)
	}
	window = model.usage.accounts[0].Windows[0]
	for _, value := range []float64{74, 75, 100} {
		window.UsedPercent = &value
		word, _ := usageStatus(window)
		want := map[float64]string{74: "OK", 75: "high", 100: "exhausted"}[value]
		if word != want {
			t.Fatalf("%v: %s", value, word)
		}
		if value == 100 && model.usagePace(window) != -1 {
			t.Fatal("exhausted pace")
		}
	}
	window = model.usage.accounts[0].Windows[0]
	window.Status = "unknown"
	for _, value := range []float64{74, 75, 97} {
		window.UsedPercent = &value
		lines := ansi.Strip(strings.Join(model.usageWindowLines(window, 50, true), "\n"))
		if model.usagePace(window) != -1 || strings.Contains(lines, "│") {
			t.Fatalf("unknown status exposed elapsed guide at %v%%: %s", value, lines)
		}
	}
	window = model.usage.accounts[0].Windows[0]
	window.Status = "exhausted"
	if model.usagePace(window) != -1 {
		t.Fatal("status-only exhaustion pace")
	}
	window = model.usage.accounts[0].Windows[0]
	expired := model.now.Add(-time.Minute)
	window.ResetsAt = &expired
	got := ansi.Strip(strings.Join(model.usageWindowLines(window, 50, true), "\n"))
	if model.usagePace(window) != -1 || !strings.Contains(got, "reset passed · awaiting traffic") || !strings.Contains(got, "20% used") {
		t.Fatal(got)
	}
	window = model.usage.accounts[0].Windows[0]
	window.WindowMinutes = nil
	if model.usagePace(window) != -1 {
		t.Fatal("unknown duration pace")
	}
	window = model.usage.accounts[0].Windows[0]
	window.LastSeenAt = nil
	if model.usagePace(window) != -1 {
		t.Fatal("unknown observation age pace")
	}
	window.Status = "unknown"
	window.UsedPercent = nil
	window.ResetsAt = nil
	got = ansi.Strip(strings.Join(model.usageWindowLines(window, 50, true), "\n"))
	if !strings.Contains(got, "No data yet") || strings.Contains(got, "0%") {
		t.Fatal(got)
	}
}

func TestDashboardUsageNoDataFailureAndExtraWindows(t *testing.T) {
	model := usageFixture(t, "normal")
	model.usageFailing = true
	got := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n"))
	if !strings.Contains(got, "feed unavailable") || !strings.Contains(got, "seen 2m") || strings.Contains(got, "stale 2m") || !strings.Contains(got, "20% used") {
		t.Fatal(got)
	}
	model = usageFixture(t, "no-data")
	got = ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n"))
	for _, want := range []string{"Claude · shaul9191 · Max", "Codex · shaul9191 · Pro", "shaul.lavochkin · Pro", "seen —", "No reading yet · waits for traffic"} {
		if !strings.Contains(got, want) {
			t.Fatal("missing", want, got)
		}
	}
	if strings.Contains(got, "rotating") || strings.Contains(got, "last served") || strings.Contains(got, "cooldown") || strings.Contains(got, "▪") {
		t.Fatal(got)
	}
	account := usageFixture(t, "normal").usage.accounts[0].Account
	account.Windows = append(account.Windows, account.Windows[0])
	projected := projectUsageAccount(account, true)
	if len(projected.Windows) != 2 || projected.extraWindows != 1 {
		t.Fatal(projected)
	}
	if got := ansi.Strip(model.usageIdentity(projected, 50, false)); !strings.Contains(got, "+1 window") {
		t.Fatal(got)
	}
}

func TestDashboardUsageMixedWindowAgeAndPublicationAge(t *testing.T) {
	model := usageFixture(t, "normal")
	old := model.now.Add(-18 * time.Minute)
	account := &model.usage.accounts[0]
	account.Windows[1].LastSeenAt = &old
	got := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n"))
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[1], "seen 2m") || !strings.Contains(lines[3], "│") || strings.Contains(lines[5], "│▪") || !strings.Contains(lines[5], "stale 18m") {
		t.Fatal(got)
	}
	result := usagefeed.Result{Snapshot: &usagefeed.Snapshot{SchemaVersion: 1, GeneratedAt: model.now.Add(time.Hour), Accounts: []usagefeed.Account{account.Account}}, Revision: 1}
	next, _ := model.Update(dashboardUsageMsg(result))
	model = next.(dashboardModel)
	if age := usageAge(model.now, model.usage.accounts[0].Windows[1].LastSeenAt); age != "stale 18m" {
		t.Fatal(age)
	}
}

func TestDashboardUsageLayoutThemesResizeAndRetainedView(t *testing.T) {
	model := usageFixture(t, "normal")
	for _, palette := range dashboardPalettes {
		model.palette = dashboardTheme(palette.name)
		got := model.render()
		assertFits(t, got, 160, 45)
		lines := strings.Split(ansi.Strip(got), "\n")
		if len(lines) != 45 || !strings.HasPrefix(string([]rune(lines[27])[106:]), "┌AI plans") {
			t.Fatalf("%s placement: %s", palette.name, got)
		}
		if model.graphHeight() != 4 {
			t.Fatalf("%s graph height %d", palette.name, model.graphHeight())
		}
		servicesTitle := string([]rune(lines[27])[58:105])
		if !strings.Contains(servicesTitle, "ready 6 · failed 0 · 1 idle") || !strings.Contains(servicesTitle, "7/7") {
			t.Fatal("service totals truncated", servicesTitle)
		}
		for _, want := range []string{"Hosts 4 / 4 visible", "macbook-air", "E8WS", "N8PF", "5173", "ai", "comfy", "life", "place", "platform", "workbench", "shaul.lavochkin"} {
			if !strings.Contains(ansi.Strip(got), want) {
				t.Fatal(palette.name, "missing", want, got)
			}
		}
		if strings.Count(ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n")), "Codex") != 1 {
			t.Fatal("provider heading repeated")
		}
	}
	model.ascii = true
	if got := ansi.Strip(model.render()); strings.ContainsAny(got, "▪●│┌┐└┘") {
		t.Fatal("ASCII retained Unicode marks")
	}
	model.ascii = false
	for _, size := range [][2]int{{80, 24}, {110, 32}, {140, 40}, {160, 45}, {60, 20}} {
		next, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model = next.(dashboardModel)
		assertFits(t, model.View().Content, size[0], size[1])
		if size[0] >= 80 && !strings.Contains(ansi.Strip(model.View().Content), "AI plans") {
			t.Fatal("configured panel lost")
		}
	}
	next, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	model = next.(dashboardModel)
	if allocations := testing.AllocsPerRun(100, func() { _ = model.View() }); allocations != 0 {
		t.Fatalf("View allocations: %v", allocations)
	}
	model.usageEnabled = false
	original := model.render()
	model.usage = dashboardUsage{}
	if model.render() != original {
		t.Fatal("disabled usage changed existing layout")
	}
}

func TestDashboardUsageFailuresRetainAttention(t *testing.T) {
	for _, size := range [][2]int{{160, 45}, {80, 24}} {
		model := usageFixture(t, "normal")
		model.width, model.height = size[0], size[1]
		host := &model.hosts[0]
		host.Services.Rows[0] = cli.DashboardService{Name: "failed-service", State: "unhealthy", Failed: true, Problem: "health check refused"}
		host.Services.Failed = 1
		host.Services.Idle = 0
		plain := ansi.Strip(model.render())
		assertFits(t, plain, size[0], size[1])
		if strings.Contains(plain, "1 idle") {
			t.Fatal("failed fixture retained its replaced idle service count", plain)
		}
		for _, want := range []string{"Attention", "failed-service", "health check refused", "Hosts 4 / 4", "E8WS", "N8PF"} {
			if !strings.Contains(plain, want) {
				t.Fatal(size, "missing", want, plain)
			}
		}
	}
}

func TestDashboardUsageNeutralValuesAndStatusMarks(t *testing.T) {
	for _, palette := range dashboardPalettes {
		model := usageFixture(t, "normal")
		model.palette = dashboardTheme(palette.name)
		terminal := vt.NewEmulator(54, 17)
		_, err := terminal.WriteString(strings.ReplaceAll(strings.Join(model.usagePanel(54, 17, false), "\n"), "\n", "\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		for y := range 17 {
			for x := range 54 {
				cell := terminal.CellAt(x, y)
				if cell == nil || !strings.ContainsAny(cell.Content, "0123456789%") {
					continue
				}
				if y == 0 {
					continue
				}
				if cell.Style.Fg != nil && fmt.Sprint(cell.Style.Fg) != fmt.Sprint(model.palette.textValue) {
					t.Fatalf("%s numeric %d,%d wears status ink: %v", palette.name, x, y, cell.Style.Fg)
				}
			}
		}
		if err := terminal.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkDashboardUsageRender(b *testing.B) {
	for _, count := range []int{3, 100, 1000} {
		b.Run(fmt.Sprintf("accounts%d", count), func(b *testing.B) {
			model := usageFixture(b, "normal")
			accounts := make([]usagefeed.Account, count)
			for index := range accounts {
				accounts[index] = model.usage.accounts[index%3].Account
				accounts[index].ID = fmt.Sprint(index)
				if index >= 3 {
					accounts[index].Provider = "generic"
				}
			}
			if count > 3 {
				accounts[count-1].Windows = make([]usagefeed.Window, 1000)
			}
			model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: accounts})
			b.ReportAllocs()
			for b.Loop() {
				_ = model.render()
			}
		})
	}
}

func TestDashboardUsageProjectionBoundsAndStableOrder(t *testing.T) {
	model := usageFixture(t, "normal")
	first, second, third := model.usage.accounts[0].Account, model.usage.accounts[1].Account, model.usage.accounts[2].Account
	projected := projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{first, second, third}})
	if projected.accounts[1].Label != "shaul9191" || projected.accounts[2].Label != "shaul.lavochkin" || projected.accounts[2].first {
		t.Fatal(projected)
	}
	accounts := make([]usagefeed.Account, 1000)
	for index := range accounts {
		accounts[index] = first
		accounts[index].ID = fmt.Sprint(index)
	}
	accounts[0].Windows = make([]usagefeed.Window, 1000)
	for index := range accounts[0].Windows {
		accounts[0].Windows[index] = first.Windows[index%2]
	}
	projected = projectDashboardUsage(&usagefeed.Snapshot{Accounts: accounts})
	if len(projected.accounts) != dashboardUsageCapacity || len(projected.accounts[0].Windows) != 2 || projected.accounts[0].extraWindows != 998 || projected.total != 1000 {
		t.Fatal("unbounded projection")
	}
	model.usage = projected
	frame := ansi.Strip(model.render())
	assertFits(t, frame, 160, 45)
	lines := strings.Split(frame, "\n")
	if model.graphHeight() != 4 || !strings.HasPrefix(string([]rune(lines[27])[106:]), "┌AI plans") || !strings.Contains(frame, "997 omitted") {
		t.Fatal("hidden accounts displaced fleet cards or graph rows", frame)
	}
	got := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n"))
	if !strings.Contains(got, "997 omitted") {
		t.Fatal(got)
	}
	model.usageFailing = true
	if got := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n")); !strings.Contains(got, "feed unavailable") || !strings.Contains(got, "997 omitted") {
		t.Fatal("failure hid omitted accounts", got)
	}
	model.profile = colorprofile.ASCII
	model.ascii = true
	next, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	model = next.(dashboardModel)
	if allocations := testing.AllocsPerRun(100, func() { _ = model.View() }); allocations != 0 {
		t.Fatal(allocations)
	}
}

func TestDashboardUsageCompactFactsStayVisible(t *testing.T) {
	model := usageFixture(t, "normal")
	model.width, model.height = 80, 24
	plain := ansi.Strip(model.render())
	for _, want := range []string{"used/left", "100%/0%", "97%/3%", "38m exhausted", "2d6h high", "stale 18m"} {
		if !strings.Contains(plain, want) {
			t.Fatal("compact facts missing", want, plain)
		}
	}
	if strings.Count(plain, "macbook-air") != 2 {
		t.Fatal("compact host or session identity shortened", plain)
	}
	window := model.usage.accounts[0].Windows[0]
	old := model.now.Add(-18 * time.Minute)
	window.LastSeenAt = &old
	if line := model.usageCompactWindow(window, 36, true); !strings.Contains(line, "stale 18m") || !strings.Contains(line, "OK") {
		t.Fatal("independent compact window age lost", line)
	}
	window = model.usage.accounts[0].Windows[1]
	expired := model.now.Add(-time.Minute)
	window.ResetsAt = &expired
	if line := model.usageCompactWindow(window, 36, false); !strings.Contains(line, "66%/34%") || !strings.Contains(line, "resets 0s passed") || ansi.StringWidth(line) > 36 {
		t.Fatal("compact reset history lost", line)
	}
	model.width, model.height = 80, 24
	model.usage.accounts[0].Windows[1] = window
	if footer := model.usageFooter(); !strings.Contains(footer, "reset passed · awaiting traffic") || !strings.Contains(footer, "used/left") {
		t.Fatal("compact reset explanation lost", footer)
	}
}
