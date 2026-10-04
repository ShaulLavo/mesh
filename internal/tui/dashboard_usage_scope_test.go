package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardUsageAccountScopes(t *testing.T) {
	model := usageFixture(t, "model-scoped")
	account := model.usage.accounts[0]
	if len(account.Windows) != 1 || account.Windows[0].ID != "weekly" || account.extraWindows != 0 {
		t.Fatalf("model quota entered account projection: %+v", account)
	}
	for _, compact := range []bool{false, true} {
		rows := model.usageAccountWindows(account, 50, compact)
		got := ansi.Strip(strings.Join(rows, "\n"))
		if strings.Count(got, "66%") != 1 || strings.Contains(got, "Model") {
			t.Fatalf("account quota repeated: %s", got)
		}
		if usageAccountRows(account, compact) != len(rows)+1 {
			t.Fatal("row budget differs from account rows")
		}
	}
}

func TestDashboardUsageModelOnlyHasNoAggregateReading(t *testing.T) {
	model := dashboardUsageFleetFixture()
	used := 100.0
	seen := model.now
	for _, id := range []string{"model:gpt-6.1-sol:weekly", "seven_day_fable", "cliproxy-passive-cache:weekly"} {
		account := projectUsageAccount(usagefeed.Account{Provider: "codex", Label: "fixture", LastSeenAt: &seen, Windows: []usagefeed.Window{{ID: id, Label: "Weekly", UsedPercent: &used, LastSeenAt: &seen, Status: usageExhausted}}}, true)
		for _, compact := range []bool{false, true} {
			got := ansi.Strip(strings.Join(model.usageAccountWindows(account, 50, compact), "\n"))
			if len(account.Windows) != 0 || account.extraWindows != 0 || strings.Contains(got, "100%") || strings.Contains(got, usageExhausted) || !strings.Contains(got, "No quota reading") {
				t.Errorf("%s promoted model reading: %s", id, got)
			}
		}
	}
}
