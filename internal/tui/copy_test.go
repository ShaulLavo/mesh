package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	m := newModel([]host{{id: "copy-owner", machineName: "pc", sessions: []session{{id: "7K3D", state: "detached", cwd: "/fixture/project", command: []string{"bash"}, createdAt: pickerTestNow}}}}, pickerTestNow)
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
		state := sessionActionState{target: sessionActionTarget{hostID: "pc", sessionID: "7K3D"}, action: action.action, phase: sessionActionRunning}
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

func TestCopyPickerFooterUsesPlainActionsAtNarrowWidths(t *testing.T) {
	for _, state := range []string{"detached", "running"} {
		for _, width := range []int{52, 64, 80} {
			t.Run(fmt.Sprintf("%s/%d", state, width), func(t *testing.T) {
				m := newModel([]host{{id: "copy-owner", machineName: "pc", sessions: []session{{id: "7K3D", state: state, cwd: "/fixture/project", command: []string{"bash"}, createdAt: pickerTestNow}}}}, pickerTestNow)
				m.showSessions()
				m = updateModel(t, m, tea.WindowSizeMsg{Width: width, Height: 24})
				footer := ansi.Strip(m.footer(m.currentHost()))
				for _, text := range []string{"k end", "x forget"} {
					if !strings.Contains(footer, text) {
						t.Errorf("footer lacks %q: %s", text, footer)
					}
				}
				view := m.View().Content
				assertFits(t, view, width, 24)
				if width == 80 && (!strings.Contains(ansi.Strip(view), "k end") || !strings.Contains(ansi.Strip(view), "x forget")) {
					t.Errorf("complete action labels did not fit at 80 columns: %s", footer)
				}
			})
		}
	}
}

func TestCopyPickerEndAndForgetKeepKeys(t *testing.T) {
	for _, width := range []int{52, 64, 80} {
		for _, action := range []struct {
			key   rune
			state string
			want  cli.PickerSessionAction
		}{
			{'k', "detached", cli.PickerKillSession},
			{'x', "exited", cli.PickerRemoveSession},
		} {
			t.Run(fmt.Sprintf("%c/%d", action.key, width), func(t *testing.T) {
				m := newModel([]host{{id: "copy-owner", machineName: "pc", sessions: []session{{id: "7K3D", state: action.state, cwd: "/fixture/project", command: []string{"bash"}, createdAt: pickerTestNow}}}}, pickerTestNow)
				var request cli.PickerSessionActionRequest
				m.act = func(_ context.Context, got cli.PickerSessionActionRequest) error {
					request = got
					return nil
				}
				m.showSessions()
				m = updateModel(t, m, tea.WindowSizeMsg{Width: width, Height: 24})
				updated, command := m.Update(runeKey(action.key))
				if command == nil {
					t.Fatal("session key did not produce its action")
				}
				_ = command()
				if request.Action != action.want || request.HostID != "copy-owner" || request.SessionID != "7K3D" {
					t.Fatalf("session key changed its target or action: %+v", request)
				}
				assertFits(t, updated.(model).View().Content, width, 24)
			})
		}
	}
}

func TestCopyPickerForgetRefusalNamesEndKey(t *testing.T) {
	m := newModel([]host{{id: "copy-owner", machineName: "pc", sessions: []session{{id: "7K3D", state: "detached"}}}}, pickerTestNow)
	m.showSessions()
	updated, command := m.Update(runeKey('x'))
	m = updated.(model)
	if command != nil || m.selection != nil || m.sessionAction.phase != sessionActionIdle {
		t.Fatal("forgetting a live session produced an action")
	}
	if m.notice != "End session 7K3D with k before forgetting it." {
		t.Errorf("forget refusal lacks its next control: %s", m.notice)
	}
}

func TestCopyRecoveryDistinguishesAbsentAndUnreadableOutput(t *testing.T) {
	for _, test := range []struct {
		name, problem, output string
	}{
		{"absent", "", "no saved output"},
		{"corrupt", "fixture recovery record is corrupt", "saved output unavailable"},
		{"unsupported", "fixture recovery record version 999 is unsupported", "saved output unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			details := savedRecoveryDetails(session{state: "interrupted", cwd: "/fixture/project", recoveryError: test.problem})
			if details.output != test.output || details.directorySource != "launch directory" || details.directory != "/fixture/project" {
				t.Errorf("saved output state or fallback = %q, %q, %q", details.output, details.directorySource, details.directory)
			}
			preview := strings.Join(details.preview, "\n")
			if !strings.Contains(preview, "Recovery opens the launch directory.") {
				t.Error("saved output state lost its launch-directory fallback")
			}
			if test.problem != "" && !strings.Contains(preview, test.problem) {
				t.Error("unreadable saved output lost its diagnostic")
			}
		})
	}
}

func TestCopyRecoveryDetailsWaitDoesNotClaimAbsence(t *testing.T) {
	m := newModel([]host{{id: "copy-owner", machineName: "pc", sessions: []session{{id: "7K3D", state: "interrupted", cwd: "/fixture/project", recoveryDetailsOmitted: true}}}}, pickerTestNow)
	m.showSessions()
	_, selected, ok := m.currentSession()
	if !ok {
		t.Fatal("fixture has no selected session")
	}
	m.inspection = inspectionState{kind: inspectionLoading}
	if output := m.savedDetailsFor(selected).output; output != "Loading previous output" {
		t.Errorf("pending saved output = %q", output)
	}
	m.inspection = inspectionState{kind: inspectionFailed, problem: "fixture saved output read failed"}
	details := m.savedDetailsFor(selected)
	if details.output != "saved output unavailable" || !strings.Contains(strings.Join(details.preview, "\n"), m.inspection.problem) {
		t.Errorf("saved output read failure = %q; diagnostic retained %t", details.output, strings.Contains(strings.Join(details.preview, "\n"), m.inspection.problem))
	}
}
