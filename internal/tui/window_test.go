package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func windowFixture() cli.WindowInput {
	return cli.WindowInput{HostID: "laptop-id", HostAlias: "laptop", HostAliases: map[string]string{"pc-id": "pc"}, Sessions: []protocol.SessionInfo{
		{ID: "91AZ", State: "running", Cwd: "/work/attached", CreatedAt: pickerTestNow},
		{ID: "Q8ME", State: "interrupted", Cwd: "/work/interrupted", CreatedAt: pickerTestNow.Add(-time.Hour)},
		{ID: "7K3D", State: "detached", Cwd: "/work/older", CreatedAt: pickerTestNow.Add(-time.Minute)},
		{ID: "BC45", State: "detached", Cwd: "/work/newest", CreatedAt: pickerTestNow},
	}}
}

func TestWindowPromptOrdersAndPreselectsDetachedSessions(t *testing.T) {
	input := windowFixture()
	current := newWindowModel(context.Background(), input, pickerTestNow)
	var ids []string
	for _, row := range current.picker.currentHost().sessions {
		ids = append(ids, row.id)
	}
	if want := []string{"BC45", "7K3D", "Q8ME", "91AZ"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("session order = %v, want %v", ids, want)
	}
	if current.picker.selectedSessionID() != "BC45" || !current.selected || input.Sessions[0].ID != "91AZ" {
		t.Fatalf("initial selection = %q, active %t; input first %q", current.picker.selectedSessionID(), current.selected, input.Sessions[0].ID)
	}
	plain := ansi.Strip(current.View().Content)
	for _, want := range []string{"Resume on laptop", "newest", "interrupted", "in use", "enter resume", "n new", "l list"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("compact view lacks %q:\n%s", want, plain)
		}
	}
	assertFits(t, current.View().Content, 80, 24)
}

func TestWindowPromptChoices(t *testing.T) {
	for _, test := range []struct {
		name string
		key  tea.KeyPressMsg
		want cli.WindowSelection
	}{
		{name: "resume", key: key(tea.KeyEnter), want: cli.WindowSelection{SessionID: "BC45"}},
		{name: "digit", key: runeKey('2'), want: cli.WindowSelection{SessionID: "7K3D"}},
		{name: "relaunch", key: runeKey('3'), want: cli.WindowSelection{SessionID: "Q8ME", Relaunch: true}},
		{name: "take over needs full picker", key: runeKey('4'), want: cli.WindowSelection{SessionID: "91AZ", FullPicker: true}},
		{name: "fresh", key: runeKey('n'), want: cli.WindowSelection{New: true}},
		{name: "full picker", key: runeKey('l'), want: cli.WindowSelection{FullPicker: true}},
		{name: "cancel", key: key(tea.KeyEscape)},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := newWindowModel(context.Background(), windowFixture(), pickerTestNow)
			program := teatest.NewTestModel(t, current, teatest.WithInitialTermSize(80, 24))
			program.Send(test.key)
			finished := program.FinalModel(t, teatest.WithFinalTimeout(time.Second)).(windowModel)
			if finished.selection == nil || *finished.selection != test.want {
				t.Fatalf("selection = %#v, want %#v", finished.selection, test.want)
			}
			assertFits(t, finished.View().Content, 80, 24)
		})
	}
}

func TestWindowPromptNeverPreselectsAttachedSession(t *testing.T) {
	input := windowFixture()
	input.Sessions = input.Sessions[:1]
	current := newWindowModel(context.Background(), input, pickerTestNow)
	if current.selected || strings.Contains(ansi.Strip(current.View().Content), "›") {
		t.Fatalf("attached session was preselected:\n%s", current.View().Content)
	}
	updated, _ := current.Update(key(tea.KeyEnter))
	if got := updated.(windowModel).selection; got == nil || !got.New || got.SessionID != "" {
		t.Fatalf("enter on attached-only prompt = %#v", got)
	}
	updated, _ = current.Update(key(tea.KeyDown))
	chosen := updated.(windowModel)
	updated, _ = chosen.Update(key(tea.KeyEnter))
	if got := updated.(windowModel).selection; got == nil || !got.FullPicker || got.SessionID != "91AZ" {
		t.Fatalf("explicit attached selection = %#v", got)
	}
}

func TestWindowPromptShowsNestedLivePreviewAndEveryStateFits(t *testing.T) {
	current := newWindowModel(context.Background(), windowFixture(), pickerTestNow)
	current.picker.inspect = func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}
	_ = current.picker.inspectSelected()
	lastOutput := pickerTestNow.Add(-12 * time.Second)
	current.picker = deliverCurrentInspection(t, current.picker, cli.SessionInspection{
		ObservedAt: pickerTestNow, LastOutputAt: &lastOutput,
		TerminalTitle: strings.Repeat("long-title-", 12),
		Nested:        []protocol.SessionIdentity{{HostID: "pc-id", SessionID: "7K3D"}},
		Preview:       []string{"old", "remote shell", "$ pwd", "/work/mesh"},
	}, nil)
	current.resize()
	plain := ansi.Strip(current.View().Content)
	for _, want := range []string{"on pc/7K3D", "output 12s ago", "remote shell", "/work/mesh"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("live window preview lacks %q:\n%s", want, plain)
		}
	}
	for _, phase := range []inspectionKind{inspectionLoading, inspectionReady, inspectionFailed, inspectionUnavailable} {
		current.picker.inspection.kind = phase
		for _, size := range [][2]int{{80, 24}, {52, 16}, {32, 8}} {
			updated, _ := current.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := updated.(windowModel).View().Content
			assertFits(t, view, size[0], size[1])
		}
	}
}

func TestWindowPromptForgetsOnlyInterruptedSession(t *testing.T) {
	for _, failure := range []bool{false, true} {
		input := windowFixture()
		calls := 0
		input.Action = func(_ context.Context, request cli.PickerSessionActionRequest) error {
			calls++
			if request.HostAlias != "laptop" || request.SessionID != "Q8ME" || request.Action != cli.PickerRemoveSession {
				t.Fatalf("forget request = %#v", request)
			}
			if failure {
				return errors.New("worker unavailable")
			}
			return nil
		}
		current := newWindowModel(context.Background(), input, pickerTestNow)
		_, command := current.Update(runeKey('x'))
		if command != nil || calls != 0 {
			t.Fatal("forget acted on a detached session")
		}
		current.picker.list.Select(2)
		updated, command := current.Update(runeKey('x'))
		current = updated.(windowModel)
		if command == nil || current.picker.sessionAction.phase != sessionActionRunning {
			t.Fatalf("forget did not start: command nil %t, phase %d", command == nil, current.picker.sessionAction.phase)
		}
		updated, _ = current.Update(command())
		current = updated.(windowModel)
		_, exists := sessionState(current.picker.currentHost().sessions, "Q8ME")
		if calls != 1 || exists != failure {
			t.Fatalf("forget calls %d, old row exists %t, failure %t", calls, exists, failure)
		}
		assertFits(t, current.View().Content, 80, 24)
	}
}

func TestWindowPromptVimNavigationDoesNotKill(t *testing.T) {
	current := newWindowModel(context.Background(), windowFixture(), pickerTestNow)
	updated, _ := current.Update(runeKey('j'))
	current = updated.(windowModel)
	if current.picker.selectedSessionID() != "7K3D" {
		t.Fatalf("j selected %q", current.picker.selectedSessionID())
	}
	updated, _ = current.Update(runeKey('k'))
	current = updated.(windowModel)
	if current.picker.selectedSessionID() != "BC45" || current.picker.sessionAction.phase != sessionActionIdle {
		t.Fatalf("k selected %q, action phase %d", current.picker.selectedSessionID(), current.picker.sessionAction.phase)
	}
}
