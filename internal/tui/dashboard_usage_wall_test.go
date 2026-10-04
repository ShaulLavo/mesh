package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestDashboardUsageWallAccounts(t *testing.T) {
	for _, size := range [][2]int{{160, 45}, {172, 46}, {344, 92}} {
		model := usageFixture(t, "wall-accounts")
		refreshWallFleetFixture(&model, time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC))
		model.width, model.height = size[0], size[1]
		if model.usagePanelWidth() < model.width*30/100 {
			t.Errorf("AI panel width %d is too narrow at %v", model.usagePanelWidth(), size)
		}
		frame := model.render()
		assertFits(t, frame, model.width, model.height)
		if *usageEvidenceDirectory != "" {
			writeUsageEvidence(t, fmt.Sprintf("wall-accounts-%dx%d", size[0], size[1]), model)
		}
		panel := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n"))
		for _, want := range []string{"3 accounts", "Claude", "account-1", "account-2", "account-3", "100% used", "43% used", "3% used", "stale 1h", "stale 2h", "stale 28m"} {
			if !strings.Contains(panel, want) {
				t.Errorf("%v missing %q:\n%s", size, want, panel)
			}
		}
		for _, unwanted := range []string{"Model", "gpt-", "Fable", "90%", "omitted"} {
			if strings.Contains(panel, unwanted) {
				t.Errorf("%v unwanted %q:\n%s", size, unwanted, panel)
			}
		}
		if strings.Count(panel, "43% used") != 1 {
			t.Errorf("repeated aggregate quota:\n%s", panel)
		}
	}
}

func refreshWallFleetFixture(model *dashboardModel, now time.Time) {
	previous := model.now
	model.now = now
	for _, host := range model.hosts {
		host.LastReply, host.NameObservedAt = model.now, model.now
		host.CPU.MeasuredAt, host.RAM.MeasuredAt, host.Uptime.MeasuredAt = model.now, model.now, model.now
		host.Sessions.ObservedAt, host.Services.ObservedAt = model.now, model.now
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
		for index := range host.Temperatures {
			host.Temperatures[index].MeasuredAt = model.now
		}
		history := model.history[host.Host.ID]
		for index := range history.cpu {
			history.cpu[index].at = history.cpu[index].at.Add(model.now.Sub(previous))
		}
		for index := range history.ram {
			history.ram[index].at = history.ram[index].at.Add(model.now.Sub(previous))
		}
		model.history[host.Host.ID] = history
		model.receive(host)
	}
}

func TestDashboardUsageLastReadingLabel(t *testing.T) {
	model := usageFixture(t, "wall-accounts")
	refreshWallFleetFixture(&model, time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC))
	account := model.usage.accounts[1]
	for _, compact := range []bool{false, true} {
		got := ansi.Strip(strings.Join(model.usageAccountWindows(account, 50, compact), "\n"))
		if strings.Contains(got, "unknown") || !strings.Contains(got, "reading") || !strings.Contains(got, "stale 28m") || !strings.Contains(got, "43%") {
			t.Errorf("ambiguous quota age/status: %s", got)
		}
	}
}

func TestDashboardUsageWallPreservesFleetFailure(t *testing.T) {
	for _, size := range [][2]int{{172, 46}, {344, 92}} {
		model := usageFixture(t, "wall-accounts")
		refreshWallFleetFixture(&model, time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC))
		host := model.hosts[0]
		host.Services.Rows[0].Failed = true
		host.Services.Rows[0].State = "unhealthy"
		host.Services.Rows[0].Problem = "health check refused"
		host.Services.Failed, host.Services.Idle = 1, 0
		model.receive(host)
		model.width, model.height = size[0], size[1]
		frame := model.render()
		assertFits(t, frame, model.width, model.height)
		plain := ansi.Strip(frame)
		for _, want := range []string{"E8WS", "N8PF", "5173", "comfy", "life", "place", "platform", "workbench", "Attention", "health check refused", "Claude", "account-1", "account-3"} {
			if !strings.Contains(plain, want) {
				t.Errorf("size %v lost %q", size, want)
			}
		}
		if *usageEvidenceDirectory != "" {
			writeUsageEvidence(t, fmt.Sprintf("wall-failure-%dx%d", size[0], size[1]), model)
		}
	}
}
