package tui

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/hostmetrics"
)

func dashboardPerformanceFixture() dashboardModel {
	model := dashboardDesignFixture(4)
	model.height = 45
	model.hosts = []cli.DashboardHostView{model.hosts[0], model.hosts[3], model.hosts[1], model.hosts[2]}
	aliases := []string{"pc", "shauls-macbook-air", "pi", "vps"}
	sessionCounts := []int{10, 3, 1, 2}
	for i := range model.hosts {
		host := &model.hosts[i]
		host.Host.Alias = aliases[i]
		host.Connection = cli.StateReachable
		host.LastReply = model.now
		host.PerformanceVersion = 1
		host.CPU.MeasuredAt = model.now
		host.RAM.MeasuredAt = model.now
		host.Sessions.ObservedAt = model.now
		host.Sessions.Failing = false
		host.Services.ObservedAt = model.now
		host.Services.Failing = false
		host.Problem = ""
		host.CPU.Value = []float64{9, 19, 5, 12}[i]
		totalGiB := []float64{31.1, 16, 3.7, 3.8}[i]
		usedGiB := []float64{18.4, 10.8, 0.5, 2.1}[i]
		host.RAM.Value.TotalBytes = uint64(totalGiB * (1 << 30))
		host.RAM.Value.AvailableBytes = host.RAM.Value.TotalBytes - uint64(usedGiB*(1<<30))
		host.Uptime.Value = []uint64{86400 + 17*3600, 35*86400 + 21*3600, 86400 + 2*3600, 41*86400 + 6*3600}[i]
		history := dashboardHostHistory{}
		for n := range 60 {
			at := model.now.Add(time.Duration(n-59) * 2 * time.Second)
			cpu := host.CPU.Value + float64((n%7)-3)
			if i == 1 {
				cpu = host.CPU.Value + 7*math.Sin(float64(n)/3)
			}
			history.cpu = append(history.cpu, dashboardPoint{at: at, sample: fmt.Sprint(n), segment: 1, value: max(0, cpu)})
			history.ram = append(history.ram, dashboardPoint{at: at, sample: fmt.Sprint(n), segment: 1, value: usedGiB/totalGiB*100 - float64(59-n)/20})
		}
		model.history[host.Host.ID] = history
		host.Services.Rows = nil
		host.Services.Total = 0
		host.Services.Ready = 0
		host.Services.Failed = 0
		if i == 0 {
			for n, name := range []string{"comfy", "5173", "ai", "life", "place", "platform", "studio", "workbench"} {
				service := cli.DashboardService{Name: name, State: "ready"}
				if n == 0 {
					service.State = "unhealthy"
					service.Failed = true
					service.Problem = "health check :8188 refused"
					host.Services.Failed++
				} else {
					host.Services.Ready++
				}
				host.Services.Rows = append(host.Services.Rows, service)
			}
			host.Services.Total = len(host.Services.Rows)
		}
		if i == 3 {
			for n := range 6 {
				host.Services.Rows = append(host.Services.Rows, cli.DashboardService{Name: fmt.Sprintf("service%d", n), State: "ready"})
			}
			host.Services.Rows[0].Name = "coolify"
			host.Services.Total = 6
			host.Services.Ready = 6
		}

		cores := make([]float64, []int{28, 8, 4, 2}[i])
		for n := range cores {
			cores[n] = min(5, host.CPU.Value)
		}
		if i == 0 {
			cores[6] = 100
		}
		host.Cores = &cli.DashboardMeasurement[[]float64]{State: statusAvailable, Value: cores, Sample: "cores", MeasuredAt: model.now}
		host.Disk = &cli.DashboardMeasurement[hostmetrics.Disk]{State: statusAvailable, Value: hostmetrics.Disk{ReadBytesPerSecond: []float64{48e6, 1.2e6, 0, 220e3}[i], WriteBytesPerSecond: []float64{12e6, 640e3, 96e3, 1.4e6}[i], BusyPercent: []float64{7, 0, 1, 3}[i], BusyAvailable: i != 1}, Sample: "disk", MeasuredAt: model.now}
		host.Network = &cli.DashboardMeasurement[hostmetrics.Network]{State: statusAvailable, Value: hostmetrics.Network{ReceiveBytesPerSecond: []float64{2.4e6, 420e3, 64e3, 180e3}[i], SendBytesPerSecond: []float64{310e3, 88e3, 21e3, 1.1e6}[i]}, Sample: "net", MeasuredAt: model.now}
		host.Sessions.Total = sessionCounts[i]
		for len(host.Sessions.Rows) < sessionCounts[i] {
			host.Sessions.Rows = append(host.Sessions.Rows, cli.DashboardSession{ID: fmt.Sprintf("%dA%dB", i, len(host.Sessions.Rows)), Name: "work", State: "detached", Command: "bash"})
		}
		host.GPU = nil
		host.Battery = nil
		host.Temperatures = nil
		if i < 2 {
			gpu := hostmetrics.GPU{Utilization: []float64{12, 18}[i], MemoryUsedBytes: 31 * (1 << 30) / 10, MemoryTotalBytes: 8 << 30}
			if i == 1 {
				gpu.MemoryKind = "shared"
				gpu.MemoryUsedBytes = 353763328
				gpu.MemoryTotalBytes = 0
			}
			host.GPU = &cli.DashboardMeasurement[hostmetrics.GPU]{State: statusAvailable, Value: gpu, Sample: "gpu", MeasuredAt: model.now}
			for _, value := range []hostmetrics.ComponentTemperature{{Kind: "cpu", Label: "CPU", Celsius: []float64{61, 52}[i]}, {Kind: "gpu", Label: "GPU", Celsius: []float64{44, 47}[i]}} {
				host.Temperatures = append(host.Temperatures, cli.DashboardMeasurement[hostmetrics.ComponentTemperature]{State: statusAvailable, Value: value, Sample: "temps", MeasuredAt: model.now})
			}
		}
		if i == 0 {
			host.Temperatures = append(host.Temperatures, cli.DashboardMeasurement[hostmetrics.ComponentTemperature]{State: statusAvailable, Value: hostmetrics.ComponentTemperature{Kind: "nvme", Label: "NVMe", Celsius: 41}, Sample: "temps", MeasuredAt: model.now})
		}
		if i == 1 {
			host.Battery = &cli.DashboardMeasurement[hostmetrics.Battery]{State: statusAvailable, Value: hostmetrics.Battery{Percent: 71, State: "discharging", SecondsRemaining: 13200}, Sample: "bat", MeasuredAt: model.now}
		}
		if i == 2 {
			host.Temperatures = []cli.DashboardMeasurement[hostmetrics.ComponentTemperature]{{State: statusAvailable, Value: hostmetrics.ComponentTemperature{Kind: "soc", Label: "SoC", Celsius: 48}, Sample: "temps", MeasuredAt: model.now}}
		}
	}
	return model
}
func TestDashboardPerformanceDesignEvidence(t *testing.T) {
	model := dashboardPerformanceFixture()
	view := model.render()
	assertFits(t, view, 160, 45)
	plain := ansi.Strip(view)
	for _, want := range []string{"VRAM 3.1 / 8.0 GiB", "DISK r 48 MB/s", "NET ↓ 2.4 MB/s", "CPU 61° · GPU 44° · NVMe 41°", "SoC 48°", "on battery 3h 40m"} {
		if !strings.Contains(plain, want) {
			t.Fatal("missing", want, plain)
		}
	}
	if model.graphHeight() != 4 || model.cardsHeight() != 23 {
		t.Fatalf("grid budget graph=%d cards=%d", model.graphHeight(), model.cardsHeight())
	}
	rows := strings.Split(plain, "\n")
	if !strings.Contains(rows[26], "Hosts 4 / 4") {
		t.Fatalf("host grid changed table budget: %s", rows[26])
	}
	for _, index := range []int{2, 3} {
		card := ansi.Strip(strings.Join(model.card(model.hosts[index], 79), "\n"))
		if strings.Contains(card, "GPU") || strings.Contains(card, "BAT") || strings.Contains(card, "VRAM") {
			t.Fatalf("absent hardware takes space: %s", card)
		}
		if len(model.card(model.hosts[index], 79)) != 11 {
			t.Fatal("absent GPU/BAT kept a row")
		}
	}
	mac := ansi.Strip(strings.Join(model.card(model.hosts[1], 79), "\n"))
	if strings.Contains(mac, "VRAM") || strings.Contains(mac, "busy") || strings.Contains(mac, "353763328") {
		t.Fatal("Mac shows dedicated memory or disk busy", mac)
	}
	if strings.Contains(plain, "this host") {
		t.Fatal("local Pi title gained a redundant label")
	}
	fmt.Printf("\nBEGIN_PERFORMANCE_160_45\n%s\nEND_PERFORMANCE_160_45\n", view)
	model.width, model.height = 80, 24
	compact := model.render()
	assertFits(t, compact, 80, 24)
	for _, want := range []string{"DISK r 48M", "NET ↓ 2.4M", "SoC 48°", "BAT 71% 3h 40m left"} {
		if !strings.Contains(ansi.Strip(compact), want) {
			t.Fatal("missing compact", want)
		}
	}
	fmt.Printf("\nBEGIN_PERFORMANCE_80_24\n%s\nEND_PERFORMANCE_80_24\n", compact)
}
func TestDashboardPerformanceOfflineAndStaleFacts(t *testing.T) {
	model := dashboardPerformanceFixture()
	host := model.hosts[1]
	host.Connection = cli.StateUnreachable
	host.LastReply = model.now.Add(-32 * time.Minute)
	view := ansi.Strip(strings.Join(model.card(host, 79), "\n"))
	for _, hidden := range []string{"GPU", "BAT", "DISK", "NET", "RAM", "CPU", "VRAM", "stale 0s"} {
		if strings.Contains(view, hidden) {
			t.Fatal("offline live reading", hidden, view)
		}
	}
	if !strings.Contains(view, "last verified reply 32m") {
		t.Fatal("missing offline evidence", view)
	}
	host = model.hosts[0]
	host.GPU.MeasuredAt = model.now.Add(-time.Minute)
	host.GPU.Failing = true
	view = ansi.Strip(model.gpuLine(host, 36, 37))
	if !strings.Contains(view, "stale 1m") {
		t.Fatal("stale GPU not aged", view)
	}
}
func TestDashboardCoreGroupingAndPalettes(t *testing.T) {
	model := dashboardPerformanceFixture()
	host := model.hosts[0]
	host.Cores.Value = make([]float64, 64)
	host.Cores.Value[63] = 100
	strip := ansi.Strip(model.coreStrip(host, 28))
	if len([]rune(strip)) > 28 || !strings.Contains(strip, "█") {
		t.Fatal("core grouping lost a pinned core", strip)
	}
	model.ascii = true
	if strings.ContainsAny(ansi.Strip(model.coreStrip(host, 28)), "▁▂▃▄▅▆▇█") {
		t.Fatal("ASCII core strip uses Unicode")
	}
	if len(dashboardPalettes) != 6 {
		t.Fatal("missing palette")
	}
	if dashboardProfileStyle(dashboardGPUStyle, colorprofile.ANSI).GetForeground() != dashboardProfileStyle(dashboardColorStyle(dashboardColor{"#b39df3", 5}), colorprofile.ANSI).GetForeground() {
		t.Fatal("GPU console role changed")
	}
}

func (m dashboardModel) card(host cli.DashboardHostView, width int) []string {
	return m.cardWithGPU(host, width, dashboardHasGPU(host))
}
