package tui

import (
	"context"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardUsageWatcherStopsWithDashboard(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	usageStarted, usageStopped, hostStopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	input := cli.DashboardInput{
		Watch: func(ctx context.Context, _ func(cli.DashboardHostView)) error {
			select {
			case <-usageStarted:
			case <-ctx.Done():
			}
			close(hostStopped)
			return nil
		},
		UsageWatch: func(ctx context.Context, publish func(usagefeed.Result)) error {
			close(usageStarted)
			publish(usagefeed.Result{Failing: true})
			<-ctx.Done()
			close(usageStopped)
			return nil
		},
	}
	if err := runDashboard(ctx, input, io.Discard, tea.WithWindowSize(160, 45)); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("dashboard failed to stop on host watch completion")
	}
	for _, stopped := range []<-chan struct{}{hostStopped, usageStopped} {
		select {
		case <-stopped:
		default:
			t.Fatal("dashboard returned before its watcher stopped")
		}
	}
}

func TestDashboardUsagePublicationsWaitForClockFrame(t *testing.T) {
	model := usageFixture(t, "normal")
	model.frame = model.render()
	previous := model.View().Content
	for _, failing := range []bool{true, false} {
		next, _ := model.Update(dashboardUsageMsg{Failing: failing})
		model = next.(dashboardModel)
		if model.View().Content != previous {
			t.Fatal("feed publication repainted between clock frames")
		}
	}
}
