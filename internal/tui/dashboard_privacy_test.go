package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/privacy"
)

func TestPrivateDashboardSkippedRestartNoticeRemainsReadable(t *testing.T) {
	const notice = "Daemon binary changed; dashboard restart skipped in privacy mode. Restart manually after recording."
	host := cli.DashboardHost{ID: "local", MachineName: "workstation", Local: true}
	model := newDashboard(cli.DashboardInput{Privacy: privacy.New(), Hosts: []cli.DashboardHost{host}}, pickerTestNow)
	model.width, model.height = 200, 50
	model.receive(cli.DashboardHostView{Host: host, Connection: cli.StateReachable, LastReply: pickerTestNow, Notice: notice})
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, notice) || !strings.Contains(view, "workstation") {
		t.Fatalf("privacy dashboard lost its readable restart notice or host: %s", view)
	}
}
