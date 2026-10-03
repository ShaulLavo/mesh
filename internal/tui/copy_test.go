package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestCopyDashboardExplainsRefresh(t *testing.T) {
	m := newDashboard(cli.DashboardInput{}, pickerTestNow)
	for _, size := range [][2]int{{80, 24}, {160, 48}} {
		m.width, m.height = size[0], size[1]
		footer := ansi.Strip(m.footer())
		for _, text := range []string{"readings every 2s", "lists update on change", "updated 12:00:00"} {
			if !strings.Contains(footer, text) {
				t.Errorf("footer at %dx%d lacks %q: %s", m.width, m.height, text, footer)
			}
		}
		assertFits(t, m.render(), m.width, m.height)
	}
}

func TestCopyPickerInspectionRetainsDiagnostic(t *testing.T) {
	m := newModel([]host{{alias: "pc", sessions: []session{{id: "7K3D", state: "detached", cwd: "/fixture/project", command: []string{"bash"}, createdAt: pickerTestNow}}}}, pickerTestNow)
	m.showSessions()
	m.inspection = inspectionState{kind: inspectionFailed, problem: "fixture connection refused"}
	_, current, ok := m.currentSession()
	if !ok {
		t.Fatal("fixture has no selected session")
	}
	details := m.detailsFor(current)
	if details.screenStatus != "Screen unavailable; retrying" || !strings.Contains(strings.Join(details.preview, "\n"), "Could not read the screen: fixture connection refused") {
		t.Fatalf("screen failure lost its explanation or diagnostic: %+v", details)
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "fixture connection refused") {
		t.Fatalf("picker details lost diagnostic: %s", view)
	}
	m.inspection = inspectionState{kind: inspectionLoading}
	if got := m.detailsFor(current).preview[0]; got != "Loading current screen…" {
		t.Errorf("loading copy = %q", got)
	}
}

func TestCopyRecoveryExplainsSavedOutput(t *testing.T) {
	details := savedRecoveryDetails(session{state: "interrupted", cwd: "/fixture/project"})
	if details.output != "no saved output" || details.directorySource != "launch directory" || details.preview[0] != "No output was saved. Recovery opens the launch directory." {
		t.Fatalf("recovery copy = %+v", details)
	}
}

func TestCopySessionActionsDescribeOutcome(t *testing.T) {
	for _, action := range []struct {
		action                     cli.PickerSessionAction
		progress, complete, failed string
	}{
		{cli.PickerKillSession, "Ending session 7K3D…", "Ended session 7K3D; refreshing…", "Could not end session 7K3D; checking its state…"},
		{cli.PickerRemoveSession, "Forgetting session 7K3D…", "Forgot session 7K3D; refreshing…", "Could not forget session 7K3D; checking its state…"},
	} {
		state := sessionActionState{target: sessionActionTarget{hostAlias: "pc", sessionID: "7K3D"}, action: action.action, phase: sessionActionRunning}
		for _, stage := range []struct {
			phase sessionActionPhase
			want  string
		}{
			{sessionActionRunning, action.progress},
			{sessionActionReconcilingSuccess, action.complete},
			{sessionActionReconcilingFailure, action.failed},
		} {
			state.phase = stage.phase
			if got := sessionActionNotice(state); got != stage.want {
				t.Errorf("action %d phase %d = %q, want %q", action.action, stage.phase, got, stage.want)
			}
		}
	}
}
