package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestDashboardDiskRatesAtWallSize(t *testing.T) {
	for _, gpu := range []bool{true, false} {
		for _, rates := range []struct {
			read, write         float64
			wantRead, wantWrite string
		}{
			{999e6, 999e6, "999MB/s", "999MB/s"},
			{953e3, 169e3, "953kB/s", "169kB/s"},
			{999.9e6, 999.9e6, "1000MB/s", "1000MB/s"},
		} {
			t.Run(fmt.Sprintf("gpu=%v/read=%g/write=%g", gpu, rates.read, rates.write), func(t *testing.T) {
				model := dashboardPerformanceFixture()
				model.width, model.height = 160, 45
				host := dashboardFixtureHost(model, "pc")
				host.Disk.Value.ReadBytesPerSecond = rates.read
				host.Disk.Value.WriteBytesPerSecond = rates.write
				host.Disk.Value.BusyAvailable = true
				host.Disk.Value.BusyPercent = 100
				host.Network.Value.ReceiveBytesPerSecond = 999.9e6
				host.Network.Value.SendBytesPerSecond = 999.9e6
				if !gpu {
					host.GPU, host.Battery = nil, nil
				}
				card := strings.Join(model.card(host, (model.width-1)/2), "\n")
				assertFits(t, card, 79, 45)
				var diskLine string
				for _, line := range strings.Split(ansi.Strip(card), "\n") {
					if strings.Contains(line, "DISK") {
						diskLine = line
					}
				}
				compact := strings.ReplaceAll(diskLine, " ", "")
				for _, want := range []string{"DISKr" + rates.wantRead, "w" + rates.wantWrite, "100%busy", "NET↓1000MB/s", "↑1000MB/s"} {
					if !strings.Contains(compact, want) {
						t.Errorf("disk reading %q was truncated: %q", want, diskLine)
					}
				}
				if strings.Contains(diskLine, "…") {
					t.Errorf("disk line contains a truncated value: %q", diskLine)
				}
			})
		}
	}
}

func TestDashboardDiskCrowdedRowPreservesBusy(t *testing.T) {
	for _, scenario := range []struct {
		name                    string
		width                   int
		staleDisk, staleNetwork bool
		wantRates               bool
	}{
		{"140 columns", 140, false, false, true},
		{"140 columns stale disk and network", 140, true, true, true},
		{"stale network", 160, false, true, true},
		{"stale disk and network", 160, true, true, true},
		{"busy priority", 100, true, true, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			model := dashboardPerformanceFixture()
			host := dashboardFixtureHost(model, "pc")
			host.Disk.Value.ReadBytesPerSecond = 999.9e6
			host.Disk.Value.WriteBytesPerSecond = 999.9e6
			host.Disk.Value.BusyAvailable = true
			host.Disk.Value.BusyPercent = 100
			host.Network.Value.ReceiveBytesPerSecond = 999.9e6
			host.Network.Value.SendBytesPerSecond = 999.9e6
			if scenario.staleDisk {
				host.Disk.MeasuredAt = model.now.Add(-10 * time.Second)
			}
			if scenario.staleNetwork {
				host.Network.MeasuredAt = model.now.Add(-10 * time.Second)
			}
			inner := (scenario.width-1)/2 - 4
			leftWidth := (inner - 2) / 2
			line := model.ioLine(host, leftWidth, inner-leftWidth-2)
			assertFits(t, line, inner, 1)
			compact := strings.ReplaceAll(ansi.Strip(line), " ", "")
			if !strings.Contains(compact, "100%busy") {
				t.Errorf("busy reading was truncated: %q", ansi.Strip(line))
			}
			if !scenario.wantRates {
				return
			}
			for _, want := range []string{"DISKr1000M", "w1000M", "NET↓1000MB/s", "↑1000MB/s"} {
				if !strings.Contains(compact, want) {
					t.Errorf("reading %q was truncated: %q", want, ansi.Strip(line))
				}
			}
		})
	}
}

func TestDashboardDisk160x45Evidence(t *testing.T) {
	model := dashboardPerformanceFixture()
	for index := range model.hosts {
		host := &model.hosts[index]
		if host.Host.MachineName != "pc" && host.Host.MachineName != "pi" {
			continue
		}
		host.Disk.Value.ReadBytesPerSecond = 999e6
		host.Disk.Value.WriteBytesPerSecond = 999e6
		host.Disk.Value.BusyAvailable = true
		host.Disk.Value.BusyPercent = 100
	}
	frame := model.render()
	assertFits(t, frame, 160, 45)
	fmt.Printf("\nBEGIN_DISK_160_45\n%s\nEND_DISK_160_45\n", frame)
}
