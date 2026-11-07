package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestConfigurationRendererGenerationResetAndRealAcknowledgement(t *testing.T) {
	model := usageFixture(t, "normal")
	acknowledgements := make(chan cli.DashboardUsageAcknowledgement, 16)
	model.usageAcknowledge = func(ack cli.DashboardUsageAcknowledgement) { acknowledgements <- ack }
	account := model.usage.accounts[0].Account
	account.Label = "Source A"
	observed := model.now.Add(-18 * time.Minute)
	account.LastSeenAt = &observed
	a := cli.DashboardUsageUpdate{Generation: 1, Enabled: true, State: "enabled", Digest: strings.Repeat("a", 64), Result: usagefeed.Result{Revision: 1, Snapshot: &usagefeed.Snapshot{GeneratedAt: model.now, Accounts: []usagefeed.Account{account}}}}
	next, _ := model.Update(dashboardUsageMsg(a))
	model = next.(dashboardModel)
	select {
	case <-acknowledgements:
		t.Fatal("acknowledged before renderer clock frame")
	default:
	}
	next, _ = model.Update(dashboardTickMsg(model.now))
	model = next.(dashboardModel)
	if got := <-acknowledgements; got != a.Acknowledgement() {
		t.Fatal(got)
	}
	if !strings.Contains(ansi.Strip(model.View().Content), "Source A") {
		t.Fatal("A never rendered")
	}
	account.Label = "Source B"
	account.Windows[0].UsedPercent = nil
	b := a
	b.Generation = 2
	b.Digest = strings.Repeat("b", 64)
	b.Result.Snapshot = &usagefeed.Snapshot{GeneratedAt: model.now, Accounts: []usagefeed.Account{account}}
	next, _ = model.Update(dashboardUsageMsg(b))
	model = next.(dashboardModel)
	next, _ = model.Update(dashboardTickMsg(model.now))
	model = next.(dashboardModel)
	frame := ansi.Strip(model.View().Content)
	if !strings.Contains(frame, "Source B") || strings.Contains(frame, "Source A") {
		t.Fatal(frame)
	}
	if model.usage.accounts[0].Windows[0].UsedPercent != nil || !model.usage.accounts[0].LastSeenAt.Equal(observed) {
		t.Fatal("unknown quota or upstream time was replaced")
	}
	if got := <-acknowledgements; got != b.Acknowledgement() {
		t.Fatal(got)
	}
	next, _ = model.Update(dashboardUsageMsg(a))
	model = next.(dashboardModel)
	if model.usageGeneration != 2 || model.usage.accounts[0].Label != "Source B" {
		t.Fatal("delayed A replaced B")
	}
	for index, state := range []string{"disabled", "enabled", "invalid", "enabled"} {
		update := cli.DashboardUsageUpdate{Generation: uint64(index + 3), Enabled: state == "enabled", State: state, Digest: strings.Repeat("c", 64)}
		next, _ = model.Update(dashboardUsageMsg(update))
		model = next.(dashboardModel)
		next, _ = model.Update(dashboardTickMsg(model.now))
		model = next.(dashboardModel)
		if model.usage.total != 0 || model.usageEnabled != (state != "disabled") {
			t.Fatalf("reset %s retained previous quota", state)
		}
		if got := <-acknowledgements; got != update.Acknowledgement() {
			t.Fatal(got)
		}
	}
}
