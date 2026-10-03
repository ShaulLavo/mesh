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

func TestDashboardUsageModelQuota(t *testing.T) {
	for _, test := range []struct {
		name, id string
		used     float64
		age      time.Duration
		reading  bool
	}{
		{"account-zero", "weekly", 0, time.Minute, true},
		{"account-partial", "weekly", 67, time.Minute, true},
		{"account-full", "weekly", 100, time.Minute, true},
		{"model-zero", "model:gpt-6.1-sol:weekly", 0, time.Minute, true},
		{"model-full", "model:gpt-6.1-sol:weekly", 100, time.Minute, true},
		{"model-stale", "model:gpt-6.1-sol:weekly", 0, 20 * time.Minute, true},
		{"model-absent", "model:gpt-6.1-sol:weekly", 0, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := dashboardUsageFleetFixture()
			seen, reset := model.now.Add(-test.age), model.now.Add(7*24*time.Hour)
			duration, active := 10080.0, false
			account := usagefeed.Account{
				ID: "synthetic", Provider: "codex", Label: "fixture", Plan: "Pro",
				State: "disabled", Source: "proxy-state", CheckedAt: &seen, LastSeenAt: &seen,
				Routing: usagefeed.Routing{Mode: "rotating", Active: &active},
				Windows: []usagefeed.Window{{ID: test.id, Label: "Weekly", Status: usageUnknown, Source: "proxy-state"}},
			}
			if test.reading {
				window := &account.Windows[0]
				window.UsedPercent, window.ResetsAt, window.WindowMinutes = &test.used, &reset, &duration
				window.LastSeenAt, window.Status = &seen, "allowed"
			}
			account = validatedUsageAccount(t, model, account)
			model.usageEnabled = true
			model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{account}})
			for _, compact := range []bool{false, true} {
				model.width, model.height = 160, 45
				panelWidth := 54
				if compact {
					model.width, model.height, panelWidth = 80, 24, 40
				}
				frame := model.render()
				assertFits(t, frame, model.width, model.height)
				if *usageEvidenceDirectory != "" {
					name := fmt.Sprintf("%s-compact-%v", test.name, compact)
					writeUsageEvidence(t, name, model)
					data, err := json.MarshalIndent(account, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					for suffix, content := range map[string]string{"-input.txt": string(data) + "\n", "-ansi.txt": frame + "\n"} {
						if err := os.WriteFile(filepath.Join(*usageEvidenceDirectory, name+suffix), []byte(content), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				panel := ansi.Strip(strings.Join(model.usagePanel(panelWidth, 17, compact), "\n"))
				t.Logf("compact=%v observed=%v projected=%d\n%s", compact, test.reading, len(model.usage.accounts[0].Windows), panel)
				if !test.reading {
					if len(model.usage.accounts[0].Windows) != 0 || !strings.Contains(panel, "no reading") || strings.Contains(panel, "0%") {
						t.Errorf("absent quota acquired a reading: %s", panel)
					}
					continue
				}
				if len(model.usage.accounts[0].Windows) != 1 {
					t.Errorf("observed quota lost during projection: %s", panel)
				}
				for _, want := range []string{"Weekly", fmt.Sprintf("%.0f%%", test.used), "resets", "7d"} {
					if !strings.Contains(panel, want) || !strings.Contains(ansi.Strip(frame), want) {
						t.Errorf("compact=%v missing %q in panel or dashboard\n%s", compact, want, panel)
					}
				}
				if strings.HasPrefix(test.id, "model:") && !strings.Contains(panel, "Model · gpt-6.1-sol") {
					t.Errorf("model scope missing: %s", panel)
				}
				age := "seen 1m"
				if test.age >= usageStaleAfter {
					age = "stale 20m"
				}
				if !strings.Contains(panel, age) || strings.Contains(panel, "no reading") {
					t.Errorf("reading age or presence lost: %s", panel)
				}
				if rows := usageAccountRows(model.usage.accounts[0], compact); rows != len(model.usageAccountWindows(model.usage.accounts[0], panelWidth-4, compact))+1 {
					t.Errorf("row budget=%d does not match rendered account", rows)
				}
			}
		})
	}
}

func TestDashboardUsageDistinctScopes(t *testing.T) {
	model := dashboardUsageFleetFixture()
	// The source fixture has equal percentages and resets for two different scopes.
	data, err := os.ReadFile("testdata/usage/model-scoped.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot usagefeed.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	account := snapshot.Accounts[0]
	account.Windows[1].Label = account.Windows[0].Label
	account = validatedUsageAccount(t, model, account)
	projected := projectUsageAccount(account, true)
	if len(projected.Windows) != 2 || projected.Windows[0].ID != "weekly" || projected.Windows[1].ID != "model:gpt-6.1-sol:weekly" {
		t.Fatalf("distinct scopes collapsed: %+v", projected.Windows)
	}
	for _, compact := range []bool{false, true} {
		rows := model.usageAccountWindows(projected, 50, compact)
		got := ansi.Strip(strings.Join(rows, "\n"))
		if strings.Count(got, "66%") != 2 || !strings.Contains(got, "Model · gpt-6.1-sol") {
			t.Errorf("equal readings need two scoped displays: %s", got)
		}
		if usageAccountRows(projected, compact) != len(rows)+1 {
			t.Errorf("scoped row budget differs from rendered rows")
		}
	}
	third := account.Windows[1]
	third.ID = "model:other:weekly"
	account.Windows = append(account.Windows, third)
	projected = projectUsageAccount(validatedUsageAccount(t, model, account), true)
	if len(projected.Windows) != 2 || projected.extraWindows != 1 {
		t.Fatalf("bounded projection hid an omitted scope: %+v", projected)
	}
}
