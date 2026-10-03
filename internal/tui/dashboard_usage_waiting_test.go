package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardUsageParkedWaiting(t *testing.T) {
	data, err := os.ReadFile("testdata/usage/parked-rotating.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture usagefeed.Snapshot
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	active, inactive := true, false
	for _, test := range []struct {
		name, state, mode string
		active            *bool
		used              float64
		age               time.Duration
		scopes            int
		absent, cooldown  bool
		waiting, evidence bool
	}{
		{name: "parked-zero", state: "disabled", mode: "rotating", active: &inactive, waiting: true, evidence: true},
		{name: "parked-partial", state: "disabled", mode: "rotating", active: &inactive, used: 67, waiting: true},
		{name: "parked-full", state: "disabled", mode: "rotating", active: &inactive, used: 100, waiting: true},
		{name: "model-zero", state: "disabled", mode: "rotating", active: &inactive, scopes: 1, waiting: true, evidence: true},
		{name: "model-stale", state: "disabled", mode: "rotating", active: &inactive, scopes: 1, age: 20 * time.Minute, waiting: true, evidence: true},
		{name: "equal-scopes", state: "disabled", mode: "rotating", active: &inactive, used: 67, scopes: 2, waiting: true},
		{name: "parked-absent", state: "disabled", mode: "rotating", active: &inactive, absent: true, waiting: true, evidence: true},
		{name: "active", state: "ready", mode: "rotating", active: &active},
		{name: "disabled-active", state: "disabled", mode: "rotating", active: &active},
		{name: "unknown-activity", state: "disabled", mode: "rotating"},
		{name: "single", state: "disabled", mode: "single", active: &inactive},
		{name: "unknown-state", state: "unknown", mode: "rotating", active: &inactive},
		{name: "unobserved", state: "no-data", mode: "rotating", absent: true},
		{name: "cooldown", state: "cooldown", mode: "rotating", active: &inactive, cooldown: true, evidence: true},
		{name: "disabled-restriction", state: "disabled", mode: "rotating", active: &inactive, cooldown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := dashboardUsageFleetFixture()
			model.now = fixture.GeneratedAt.Add(time.Minute)
			account := fixture.Accounts[0]
			account.State, account.Routing.Mode, account.Routing.Active = test.state, test.mode, test.active
			window := account.Windows[0]
			window.UsedPercent = &test.used
			if test.scopes > 0 {
				window.ID = "model:gpt-6.1-sol:weekly"
			}
			if test.age > 0 {
				seen := model.now.Add(-test.age)
				account.LastSeenAt, window.LastSeenAt = &seen, &seen
			}
			account.Windows = []usagefeed.Window{window}
			if test.scopes == 2 {
				accountWindow := window
				accountWindow.ID = "weekly"
				account.Windows = []usagefeed.Window{accountWindow, window}
			}
			if test.absent {
				account.LastSeenAt = nil
				account.Windows = []usagefeed.Window{{ID: "weekly", Label: "Weekly", Status: usageUnknown, Source: "proxy-state"}}
			}
			if test.cooldown {
				account.Cooldown = &usagefeed.Cooldown{Reason: "unauthorized", ObservedAt: model.now, Source: "proxy-state"}
			}
			account = validatedUsageAccount(t, model, account)
			before, err := json.Marshal(account)
			if err != nil {
				t.Fatal(err)
			}
			model.usageEnabled = true
			model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{account}})
			for _, size := range [][2]int{{160, 45}, {80, 24}, {160, 24}} {
				model.width, model.height = size[0], size[1]
				compact := size[0] < 140 || size[1] < 40
				panelWidth := 54
				if compact {
					panelWidth = (size[0] + 1) / 2
				}
				frame := model.render()
				assertFits(t, frame, size[0], size[1])
				if test.evidence {
					writeParkedUsageEvidence(t, fmt.Sprintf("%s-%dx%d", test.name, size[0], size[1]), model, account)
				}
				panel := ansi.Strip(strings.Join(model.usagePanel(panelWidth, 17, compact), "\n"))
				t.Logf("size=%v state=%s mode=%s waiting=%v\n%s", size, test.state, test.mode, test.waiting, panel)
				if strings.Contains(panel, "Waiting") != test.waiting || strings.Contains(ansi.Strip(frame), "Waiting") != test.waiting {
					t.Errorf("parked status must follow confirmed inactive rotating state: %s", panel)
				}
				age := "seen 1m"
				if test.age > 0 {
					age = "stale 20m"
				}
				if test.absent {
					age = "seen —"
					if strings.Contains(panel, "0%") || !strings.Contains(strings.ToLower(panel), "no reading") || len(model.usage.accounts[0].Windows) != 0 {
						t.Errorf("absent observation acquired quota or lost absence: %s", panel)
					}
				}
				for _, surface := range []string{panel, ansi.Strip(frame)} {
					if !strings.Contains(surface, age) {
						t.Errorf("observation age missing: %s", surface)
					}
					if !test.absent && (!strings.Contains(surface, fmt.Sprintf("%.0f%%", test.used)) || !strings.Contains(surface, "resets 6d") || strings.Contains(surface, "no reading")) {
						t.Errorf("observed quota or reset lost: %s", surface)
					}
					if test.scopes > 0 && !strings.Contains(surface, "Model · gpt-6.1-sol") {
						t.Errorf("model scope missing: %s", surface)
					}
				}
				if test.scopes == 2 && strings.Count(panel, "67%") != 2 {
					t.Errorf("equal values collapsed distinct scopes: %s", panel)
				}
				projected := model.usage.accounts[0]
				if usageAccountRows(projected, compact) != len(model.usageAccountWindows(projected, panelWidth-4, compact))+1 {
					t.Error("row budget changed")
				}
				if !strings.Contains(ansi.Strip(frame), ansi.Strip(model.usageFooter())) {
					t.Error("footer missing or changed")
				}
			}
			after, err := json.Marshal(account)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Error("rendering changed source state, routing or observations")
			}
		})
	}
}

func writeParkedUsageEvidence(t *testing.T, name string, model dashboardModel, account usagefeed.Account) {
	t.Helper()
	if *usageEvidenceDirectory == "" {
		return
	}
	if err := os.MkdirAll(*usageEvidenceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeUsageEvidence(t, name, model)
	data, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for suffix, content := range map[string]string{"-input.json": string(data) + "\n", "-ansi.txt": model.render() + "\n"} {
		if err := os.WriteFile(filepath.Join(*usageEvidenceDirectory, name+suffix), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
