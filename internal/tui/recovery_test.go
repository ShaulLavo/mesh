package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func savedPickerSession() protocol.SessionInfo {
	return protocol.SessionInfo{ID: "7K3D", State: "interrupted", Command: []string{"/bin/bash"}, Cwd: "/launch", CreatedAt: pickerTestNow.Add(-time.Hour), Recovery: &recovery.Record{
		CheckpointAt: pickerTestNow.Add(-time.Minute), Title: "Project tests", ShellDirectory: "/work/project", DirectorySource: recovery.DirectoryShell,
		Lines: []string{"earlier output", "tests completed"},
	}}
}

func TestAgentRecoveryKeepsLiveShellAsDefault(t *testing.T) {
	row := savedPickerSession()
	row.State = "detached"
	row.Recovery.Agent = &agentresume.Recipe{Launch: agentresume.Launch{Provider: agentresume.Claude, Directory: "/work/project"}, ConversationID: "saved-conversation", Lifecycle: agentresume.Closed}
	current := recoveryPicker(row)
	current = updateModel(t, current, key(tea.KeyEnter))
	if selected := cliSelection(current.selection); selected.Relaunch || selected.SessionID != row.ID {
		t.Fatalf("Enter did not attach the surviving shell: %#v", selected)
	}
	current = recoveryPicker(row)
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "Resume conversation") {
		t.Fatalf("missing explicit conversation action:\n%s", view)
	}
	current = updateModel(t, current, runeKey('a'))
	if selected := cliSelection(current.selection); !selected.Relaunch || selected.RecoveryAction != recovery.ActionAgent || selected.SessionID != row.ID {
		t.Fatalf("explicit conversation selection = %#v", selected)
	}
}

func TestAgentRecoveryLabelsAndUnverifiedStatus(t *testing.T) {
	row := savedPickerSession()
	row.AgentStatus = "unverified"
	row.Recovery.Agent = &agentresume.Recipe{Launch: agentresume.Launch{Provider: agentresume.Codex, Directory: "/work/project"}, ConversationID: "exact-id", Lifecycle: agentresume.Active}
	current := recoveryPicker(row)
	view := ansi.Strip(current.View().Content)
	for _, expected := range []string{"Resume codex", "exact-id", "unverified"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing %q in agent recovery:\n%s", expected, view)
		}
	}
	current = updateModel(t, current, runeKey('a'))
	if selected := cliSelection(current.selection); selected.RecoveryAction != recovery.ActionAgent {
		t.Fatalf("agent recovery selection = %#v", selected)
	}
}

func TestAgentRecoveryCloneDoesNotShareOptions(t *testing.T) {
	original := &recovery.Record{Agent: &agentresume.Recipe{Launch: agentresume.Launch{Options: []string{"--model", "original"}}}, AgentResume: &agentresume.Receipt{ConversationID: "original"}}
	cloned := cloneRecovery(original)
	cloned.Agent.Options[1] = "changed"
	cloned.AgentResume.ConversationID = "changed"
	if original.Agent.Options[1] != "original" || original.AgentResume.ConversationID != "original" {
		t.Fatal("picker clone mutated the catalog agent recipe")
	}
}

func TestInterruptedUnverifiedConversationKeepsItsRecoveryActions(t *testing.T) {
	row := savedPickerSession()
	row.AgentStatus = "unverified"
	row.Command = []string{"/mesh", "agent-resume"}
	current := recoveryPicker(row)
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "Resume conversation") {
		t.Fatalf("pending conversation was mislabeled as shell recovery:\n%s", view)
	}
	current = updateModel(t, current, runeKey('a'))
	if selected := cliSelection(current.selection); !selected.Relaunch || selected.RecoveryAction != recovery.ActionAgent || selected.SessionID != row.ID {
		t.Fatalf("pending conversation action = %#v", selected)
	}
}

func recoveryPicker(current protocol.SessionInfo) model {
	return newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: cli.HostRecord{Alias: "local"}, Local: true, Sessions: []protocol.SessionInfo{current}}}, OpenHostAlias: "local",
	}, pickerTestNow)
}

func TestSavedPreviewIsImmediateAndNeverLive(t *testing.T) {
	current := recoveryPicker(savedPickerSession())
	calls := 0
	current.inspect = func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		calls++
		return cli.SessionInspection{}, nil
	}
	if command := current.inspectSelected(); command != nil || calls != 0 {
		t.Fatal("saved preview attempted live inspection")
	}
	view := ansi.Strip(current.View().Content)
	for _, expected := range []string{"Previous output", "Project tests", "tests completed", "/work/project", savedPickerSession().Recovery.CheckpointAt.Format(time.RFC3339), "Recover shell"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing %q:\n%s", expected, view)
		}
	}
	if strings.Contains(view, "live") || strings.Contains(view, "Loading") {
		t.Fatalf("saved output claimed to be live:\n%s", view)
	}
	current = updateModel(t, current, runeKey(' '))
	if view = ansi.Strip(current.View().Content); !strings.Contains(view, "Previous output") || !strings.Contains(view, "tests completed") {
		t.Fatalf("full saved preview lost its label:\n%s", view)
	}
	assertFits(t, current.View().Content, 80, 24)
}

func TestRecoveryActionsRequireExplicitCommandSelection(t *testing.T) {
	for _, state := range []string{"interrupted", "exited"} {
		t.Run(state, func(t *testing.T) { testRecoveryActions(t, state) })
	}
}

func testRecoveryActions(t *testing.T, state string) {
	t.Helper()
	for _, test := range []struct {
		key    tea.KeyPressMsg
		action recovery.Action
	}{
		{key(tea.KeyEnter), recovery.ActionDefault}, {runeKey('s'), recovery.ActionShell}, {runeKey('c'), recovery.ActionCommand},
	} {
		row := savedPickerSession()
		row.State, row.Command = state, []string{"server", "--port", "8080"}
		current := recoveryPicker(row)
		if !strings.Contains(ansi.Strip(current.View().Content), "Open shell") {
			t.Fatal("arbitrary program default is not Open shell")
		}
		current = updateModel(t, current, test.key)
		got := cliSelection(current.selection)
		if !got.Relaunch || got.RecoveryAction != test.action || got.SessionID != row.ID || got.TakeOver {
			t.Fatalf("recovery selection = %#v", got)
		}
	}
}

func TestSavedRemoteRecoveryHasExplicitShellAlternative(t *testing.T) {
	row := savedPickerSession()
	row.Recovery.Remote = &recovery.Target{HostID: "remote-host", SessionID: "91AZ"}
	current := recoveryPicker(row)
	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "Reconnect to target") || !strings.Contains(view, "remote-host/91AZ") {
		t.Fatalf("missing exact remote target:\n%s", view)
	}
	current = updateModel(t, current, runeKey('s'))
	if got := cliSelection(current.selection); got.RecoveryAction != recovery.ActionShell {
		t.Fatalf("shell alternative = %#v", got)
	}
}

func TestSavedRemoteTargetRemainsVisibleAfterAnEarlierAgent(t *testing.T) {
	row := savedPickerSession()
	row.Recovery.Remote = &recovery.Target{HostID: "remote-host", SessionID: "91AZ"}
	row.Recovery.Agent = &agentresume.Recipe{Launch: agentresume.Launch{Provider: agentresume.Claude, Directory: "/old-agent-project"}, ConversationID: "old-conversation", Lifecycle: agentresume.Closed}
	current := recoveryPicker(row)
	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "remote-host/91AZ") {
		t.Fatalf("earlier agent hides the default remote recovery target:\n%s", view)
	}
	_, selected, ok := current.currentSession()
	if !ok || strings.Contains(sessionLabel(selected), "old-agent-project") {
		t.Fatalf("remote task is labeled with the earlier agent directory: %s", sessionLabel(selected))
	}
}

func TestRecoveryAttemptGroupingRetainsChainsAndCycles(t *testing.T) {
	sessions := []session{
		{id: "old", state: "interrupted", replacementID: "middle"},
		{id: "other", state: "detached"},
		{id: "middle", state: "exited", replacementID: "new"},
		{id: "new", state: "detached"},
	}
	grouped := groupRecoveryAttempts(sessions)
	var ids []string
	for _, current := range grouped {
		ids = append(ids, current.id)
	}
	if !reflect.DeepEqual(ids, []string{"other", "new", "middle", "old"}) || !grouped[2].previousAttempt || !grouped[3].previousAttempt {
		t.Fatalf("grouping = %#v", grouped)
	}
	cycles := groupRecoveryAttempts([]session{{id: "one", state: "exited", replacementID: "two"}, {id: "two", state: "exited", replacementID: "one"}})
	if len(cycles) != 2 {
		t.Fatalf("cycle lost records: %#v", cycles)
	}
}

func TestRecoveryFallbackAndCorruptRecordStayVisible(t *testing.T) {
	row := savedPickerSession()
	row.Recovery.DirectorySource = recovery.DirectoryObserved
	row.Recovery.Lines = []string{"\x1b[31munsafe"}
	current := recoveryPicker(row)
	details := current.detailsFor(current.currentHost().sessions[0])
	if details.directorySource != "observed fallback" || strings.Contains(details.preview[0], "\x1b") {
		t.Fatalf("unsafe saved display: %#v", details)
	}
	row.Recovery, row.RecoveryError = nil, "unsupported checkpoint version"
	current = recoveryPicker(row)
	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "unsupported checkpoint version") || !strings.Contains(view, "Previous output unavailable") || !strings.Contains(view, "launch-only") {
		t.Fatalf("corrupt record hidden:\n%s", view)
	}
}

func TestLaunchOnlyRecoveryDoesNotInventCheckpointTime(t *testing.T) {
	row := savedPickerSession()
	row.Recovery.CheckpointAt = time.Time{}
	row.Recovery.Lines = nil
	row.Recovery.Restart = &recovery.Command{Argv: []string{"make", "serve"}, Cwd: row.Cwd}
	current := recoveryPicker(row)
	view := ansi.Strip(current.View().Content)
	for _, expected := range []string{"launch-only", "Previous output unavailable", "make serve"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing %q:\n%s", expected, view)
		}
	}
	if strings.Contains(view, "0001") || strings.Contains(view, "saved 106751") {
		t.Fatalf("invented checkpoint time:\n%s", view)
	}
}

func TestWindowKeepsPreviousAttemptsInFullPicker(t *testing.T) {
	old := savedPickerSession()
	old.ReplacementID = "91AZ"
	input := cli.WindowInput{HostAlias: "local", Sessions: []protocol.SessionInfo{old, {ID: "91AZ", State: "running", RecoveredFrom: old.ID}, {ID: "Q8ME", State: "exited"}}}
	current := newWindowModel(context.Background(), input, pickerTestNow)
	if len(current.picker.currentHost().sessions) != 1 || current.selected {
		t.Fatalf("window selected previous or ended attempt: %#v", current)
	}
	updated, _ := current.Update(key(tea.KeyEnter))
	if selection := updated.(windowModel).selection; selection == nil || !selection.New {
		t.Fatalf("window restarted prior attempt: %#v", selection)
	}
}
