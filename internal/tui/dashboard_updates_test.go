package tui

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"io"
	"reflect"
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

func TestFleetDriftClockFrameBoundedWithdrawnAndIndependent(t *testing.T) {
	now := time.Now()
	model := dashboardTickFixture(t, true, 4)
	model.now = now
	initial, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	model = initial.(dashboardModel)
	plain := model.render()
	next, _ := model.Update(dashboardUpdatesMsg(cli.DashboardUpdates{Generation: 1}))
	model = next.(dashboardModel)
	if model.render() != plain {
		t.Fatal("no enrollment reserved a blank update row")
	}
	source := cli.DashboardUpdateSource{Coordinator: update.Host{ID: model.hosts[0].Host.ID}, Automatic: &cli.DashboardStatus[update.AutomaticStatus]{Observation: cli.DashboardStatusObservation{State: cli.DashboardStatusObserved, At: now}, Value: &update.AutomaticStatus{State: "stopped", Record: &update.AutomaticRecord{Stop: &update.AutomaticStop{Kind: "direct_transition_blocked"}, Current: &update.Run{Release: release.Manifest{Version: "v0.2.0"}, Targets: []update.Target{{Host: update.Host{ID: model.hosts[0].Host.ID}, State: update.Offline}}}}}}}
	model.hosts[0].Connection = cli.StateRefused
	oldUsage := model.usage
	oldServices := model.hosts[0].Services
	previous := model.View().Content
	next, _ = model.Update(dashboardUpdatesMsg(cli.DashboardUpdates{Generation: 2, Sources: []cli.DashboardUpdateSource{source}}))
	model = next.(dashboardModel)
	if model.View().Content != previous {
		t.Fatal("update publication painted outside clock frame")
	}
	for _, size := range [][2]int{{80, 24}, {140, 40}, {160, 45}, {210, 60}} {
		next, _ = model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model = next.(dashboardModel)
		assertFits(t, model.View().Content, size[0], size[1])
		frame := ansi.Strip(model.View().Content)
		if !strings.Contains(frame, "Updates") || !strings.Contains(frame, "manual bridge") {
			t.Fatal(frame)
		}
	}
	if model.hosts[0].Connection != cli.StateRefused || !reflect.DeepEqual(oldServices, model.hosts[0].Services) || !reflect.DeepEqual(oldUsage, model.usage) {
		t.Fatal("status changed identity, catalog, quota or observation age")
	}
	next, _ = model.Update(dashboardUpdatesMsg(cli.DashboardUpdates{Generation: 3}))
	model = next.(dashboardModel)
	next, _ = model.Update(dashboardUpdatesMsg(cli.DashboardUpdates{Generation: 2, Sources: []cli.DashboardUpdateSource{source}}))
	model = next.(dashboardModel)
	next, _ = model.Update(dashboardTickMsg(now))
	model = next.(dashboardModel)
	if strings.Contains(ansi.Strip(model.View().Content), "Updates") {
		t.Fatal("late generation restored withdrawn status")
	}
}
func TestDashboardUpdateReaderJoinsOnProgramClose(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, joined := make(chan struct{}), make(chan struct{})
	input := cli.DashboardInput{Watch: func(ctx context.Context, _ func(cli.DashboardHostView)) error { <-ctx.Done(); return nil }, UpdateWatch: func(ctx context.Context, publish func(cli.DashboardUpdates)) error {
		defer close(joined)
		publish(cli.DashboardUpdates{Generation: 1, Problem: "Status unavailable"})
		close(started)
		<-ctx.Done()
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- runDashboard(ctx, input, io.Discard, tea.WithWindowSize(80, 24)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Error("status reader retained")
		}
	})
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("program failed to join status watcher")
	}
	select {
	case <-joined:
	default:
		t.Fatal("program returned before update reader")
	}
}
