package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardLiveSessionNamesRefreshAndRetainStaleFacts(t *testing.T) {
	failed := false
	input := cli.DashboardInput{Inspect: func(_ context.Context, request cli.PickerInspectRequest) (cli.SessionInspection, error) {
		if request.PreviewCols != 1 || request.PreviewRows != 1 {
			t.Fatal("summary requested a full terminal preview")
		}
		if failed {
			return cli.SessionInspection{}, errors.New("unavailable")
		}
		return cli.SessionInspection{CurrentDirectory: "/work/projects/mesh", ForegroundCommand: "claude", TerminalTitle: "Fix dashboard names"}, nil
	}}
	model := newDashboard(input, time.Now())
	model.width, model.height = 160, 48
	host := cli.DashboardHostView{Host: cli.DashboardHost{ID: "pc", Alias: "pc"}, Connection: cli.StateReachable, LastReply: model.now,
		Sessions: cli.DashboardCatalog[cli.DashboardSession]{Total: 1, ObservedAt: model.now, Rows: []cli.DashboardSession{{ID: "7K3D", Name: "mesh", State: "detached", Command: "sh -c wrapper"}}}}
	model.hosts = []cli.DashboardHostView{host}
	command := model.inspectSessions()
	if command == nil || model.inspectSessions() != nil {
		t.Fatal("summary request missing or overlapping")
	}
	updated, _ := model.Update(command())
	model = updated.(dashboardModel)
	name, activity, _ := model.sessionDescription(host, host.Sessions.Rows[0])
	if name != "Fix dashboard names" || activity != "Claude" {
		t.Fatalf("live facts not used: %q / %q", name, activity)
	}
	host.Sessions.Rows[0].Label = "My terminal"
	model.hosts[0] = host
	name, _, _ = model.sessionDescription(host, host.Sessions.Rows[0])
	if name != "My terminal" {
		t.Fatalf("explicit label replaced: %q", name)
	}
	failed = true
	model.now = model.now.Add(40 * time.Second)
	host.LastReply, host.Sessions.ObservedAt = model.now, model.now
	model.hosts[0] = host
	updated, _ = model.Update(model.inspectSessions()())
	model = updated.(dashboardModel)
	_, activity, _ = model.sessionDescription(host, host.Sessions.Rows[0])
	if activity != "Claude · stale" {
		t.Fatalf("failed observation lost or misrepresented old facts: %q", activity)
	}
	host.Sessions.Rows = nil
	updated, _ = model.Update(dashboardHostMsg(host))
	model = updated.(dashboardModel)
	if len(model.sessionSummaries) != 0 {
		t.Fatal("removed session retained its live summary")
	}
}

func TestDashboardInspectsOnlyVisibleLiveRowsWithinBudget(t *testing.T) {
	model := dashboardDesignFixture(4)
	for index := range model.hosts {
		host := &model.hosts[index]
		for n := range 6 {
			host.Sessions.Rows = append(host.Sessions.Rows, cli.DashboardSession{ID: fmt.Sprintf("%04d", n)})
		}
	}
	for _, size := range [][2]int{{160, 48}, {80, 24}, {50, 12}} {
		model.width, model.height = size[0], size[1]
		targets := model.visibleSessionTargets()
		if len(targets) > 6 || len(targets) > model.visibleSessionLimit() {
			t.Fatalf("inspection exceeded visible row budget at %v: %d", size, len(targets))
		}
		for target := range targets {
			if target.hostID == "macbook" {
				t.Fatal("unreachable cached host inspected")
			}
		}
	}
}

func TestDashboardCompactSessionsKeepTheirPurpose(t *testing.T) {
	model := newDashboard(cli.DashboardInput{}, time.Now())
	row := ansi.Strip(model.usageSessionColumns("pc", "7K3D", "mesh", "detached", "Claude", 39, 4))
	if !strings.Contains(row, "mesh · Claude") {
		t.Fatalf("compact row lost session identity and activity: %s", row)
	}
}

func TestDashboardSessionNameEvidence(t *testing.T) {
	if *usageEvidenceDirectory == "" {
		t.Skip("pass -usage-evidence-dir to export the session fixture")
	}
	model := dashboardDesignFixture(4)
	host := &model.hosts[0]
	host.Sessions.Rows = []cli.DashboardSession{
		{ID: "3KM7", Name: "mesh", State: "detached", Command: "sh -c wrapper"},
		{ID: "E8WS", Name: "serve /ai", Label: "serve /ai", State: "running", Command: "bash -lc /work/cli-proxy-api/run"},
		{ID: "K02N", Name: "platform", State: "detached", Command: "sh -c wrapper"},
		{ID: "M3MN", Name: "mesh", State: "detached", Command: "/bin/bash"},
	}
	host.Sessions.Total = 4
	model.sessionSummaries[dashboardSessionTarget{host.Host.ID, "3KM7"}] = sessionLiveSummary{terminalTitle: "Fix dashboard names", foregroundCommand: "claude", receivedAt: model.now}
	model.sessionSummaries[dashboardSessionTarget{host.Host.ID, "E8WS"}] = sessionLiveSummary{foregroundCommand: "cli-proxy-api", receivedAt: model.now}
	model.sessionSummaries[dashboardSessionTarget{host.Host.ID, "K02N"}] = sessionLiveSummary{foregroundCommand: "codex", currentDirectory: "/work/projects/platform", receivedAt: model.now}
	model.sessionSummaries[dashboardSessionTarget{host.Host.ID, "M3MN"}] = sessionLiveSummary{foregroundCommand: "bash", receivedAt: model.now}
	writeUsageEvidence(t, "session-names", model)
}
