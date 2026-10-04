package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardUsageCreditFreshness(t *testing.T) {
	for _, size := range [][2]int{{172, 46}, {344, 92}, {80, 24}} {
		model := usageFixture(t, "credits-accounts")
		refreshWallFleetFixture(&model, time.Date(2026, 10, 4, 15, 45, 0, 0, time.UTC))
		model.width, model.height = size[0], size[1]
		frame := model.render()
		assertFits(t, frame, model.width, model.height)
		if *usageEvidenceDirectory != "" {
			writeUsageEvidence(t, fmt.Sprintf("credits-freshness-%dx%d", size[0], size[1]), model)
		}
		plain := ansi.Strip(frame)
		for _, want := range []string{"Credits 54,708.89", "Credits 0", "account-1", "account-2", "account-3", "Claude", "checked 2m", "5-hour", "Weekly", "stale 3h"} {
			if !strings.Contains(plain, want) {
				t.Errorf("size %v missing %q", size, want)
			}
		}
		if strings.Contains(plain, "Session 3%") || strings.Contains(plain, "read 2m") {
			t.Errorf("ambiguous check/quota labels:\n%s", plain)
		}
	}
}

func TestDashboardUsageKnownCreditPrecision(t *testing.T) {
	for _, item := range []struct {
		credits *usagefeed.Credits
		want    string
	}{
		{nil, ""}, {&usagefeed.Credits{}, "Credits 0"},
		{&usagefeed.Credits{Balance: 0.004}, "Credits <0.01"},
		{&usagefeed.Credits{Balance: 54708.8922325}, "Credits 54,708.89"},
		{&usagefeed.Credits{Balance: 12.4}, "Credits 12.40"},
		{&usagefeed.Credits{Unlimited: true}, "Credits unlimited"},
	} {
		if got := usageCredits(item.credits); got != item.want {
			t.Errorf("credit balance %q, want %q", got, item.want)
		}
	}
}

func TestDashboardUsageThreeAccountsWithFullQuotasAndCredits(t *testing.T) {
	model := usageFixture(t, "normal")
	model.width, model.height = 172, 46
	model.usage.accounts[1].Credits = &usagefeed.Credits{Balance: 54708.8922325}
	model.usage.accounts[2].Credits = &usagefeed.Credits{}
	frame := model.render()
	assertFits(t, frame, model.width, model.height)
	if *usageEvidenceDirectory != "" {
		writeUsageEvidence(t, "credits-full-quotas", model)
	}
	plain := ansi.Strip(frame)
	for _, want := range []string{"3 accounts", "Claude", "shaul.lavochkin", "Credits 54,708.89", "Credits 0"} {
		if !strings.Contains(plain, want) {
			t.Errorf("full quotas hide %q", want)
		}
	}
}
