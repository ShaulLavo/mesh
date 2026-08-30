package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/shaul/mesh/internal/cli"
)

var pickerTestNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

func TestPickerFlows(t *testing.T) {
	tests := []struct {
		name string
		keys []tea.KeyPressMsg
		want selection
	}{
		{
			name: "navigate and attach",
			keys: []tea.KeyPressMsg{key(tea.KeyEnter), key(tea.KeyDown), key(tea.KeyEnter)},
			want: attachSelection{hostAlias: "pc", sessionID: "91AZ"},
		},
		{
			name: "new session",
			keys: []tea.KeyPressMsg{key(tea.KeyEnter), runeKey('n')},
			want: newSelection{hostAlias: "pc"},
		},
		{
			name: "resume latest",
			keys: []tea.KeyPressMsg{key(tea.KeyEnter), runeKey('r')},
			want: resumeSelection{hostAlias: "pc"},
		},
		{
			name: "offline wake",
			keys: []tea.KeyPressMsg{key(tea.KeyDown), key(tea.KeyEnter), runeKey('w')},
			want: wakeSelection{hostAlias: "pi"},
		},
		{
			name: "escape back and cancel",
			keys: []tea.KeyPressMsg{key(tea.KeyEnter), key(tea.KeyEscape), key(tea.KeyEscape)},
			want: cancelSelection{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initial := newModel(pickerFixture(), pickerTestNow)
			program := teatest.NewTestModel(t, initial, teatest.WithInitialTermSize(80, 24))
			for _, pressed := range test.keys {
				program.Send(pressed)
			}
			final := program.FinalModel(t, teatest.WithFinalTimeout(time.Second))
			finished, ok := final.(model)
			if !ok {
				t.Fatalf("final model has type %T", final)
			}
			if finished.selection != test.want {
				t.Fatalf("selection = %#v, want %#v", finished.selection, test.want)
			}
			view := finished.View().Content
			assertFits(t, view, 80, 24)
			golden.RequireEqual(t, cleanSnapshot(view))
		})
	}
}

func TestPickerResizeAndEmptyStates(t *testing.T) {
	empty := newModel(nil, pickerTestNow)
	empty = updateModel(t, empty, tea.WindowSizeMsg{Width: 80, Height: 24})
	if view := empty.View().Content; !strings.Contains(view, "No hosts") {
		t.Fatalf("empty host view does not explain the empty state:\n%s", view)
	}

	withoutSessions := newModel([]host{{alias: "empty"}}, pickerTestNow)
	withoutSessions = updateModel(t, withoutSessions, key(tea.KeyEnter))
	withoutSessions = updateModel(t, withoutSessions, tea.WindowSizeMsg{Width: 52, Height: 16})
	view := withoutSessions.View().Content
	if !strings.Contains(view, "No sessions") || !strings.Contains(view, "n new") {
		t.Fatalf("empty session view lacks actions:\n%s", view)
	}
	assertFits(t, view, 52, 16)
}

func TestNonTerminalPickerReturnsWithoutStartingTea(t *testing.T) {
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	defer outputWriter.Close()

	pick := NewCLIPicker(input, outputWriter)
	started := time.Now()
	_, err = pick(context.Background(), cli.PickerInput{})
	if !errors.Is(err, ErrNonInteractive) {
		t.Fatalf("error = %v, want ErrNonInteractive", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("non-terminal picker returned after %s", elapsed)
	}
}

func TestCLISelectionMapping(t *testing.T) {
	tests := []struct {
		input selection
		want  cli.PickerSelection
	}{
		{input: cancelSelection{}, want: cli.PickerSelection{}},
		{input: attachSelection{hostAlias: "pc", sessionID: "7K3D"}, want: cli.PickerSelection{HostAlias: "pc", SessionID: "7K3D"}},
		{input: newSelection{hostAlias: "pc"}, want: cli.PickerSelection{HostAlias: "pc", New: true}},
		{input: resumeSelection{hostAlias: "pc"}, want: cli.PickerSelection{HostAlias: "pc"}},
		{input: wakeSelection{hostAlias: "pi"}, want: cli.PickerSelection{HostAlias: "pi", Wake: true}},
	}
	for _, test := range tests {
		if got := cliSelection(test.input); got != test.want {
			t.Fatalf("cliSelection(%#v) = %#v, want %#v", test.input, got, test.want)
		}
	}
}

func pickerFixture() []host {
	return []host{
		{
			alias: "pc",
			sessions: []session{
				{id: "7K3D", state: "detached", command: []string{"claude", "--dangerously-skip-permissions"}, cwd: "/home/shaul/src/mesh", createdAt: pickerTestNow.Add(-4 * time.Minute)},
				{id: "91AZ", state: "running", command: []string{"npm", "run", "dev"}, cwd: "/home/shaul/src/site", createdAt: pickerTestNow.Add(-8 * time.Minute)},
			},
		},
		{
			alias: "pi",
			stale: true,
			sessions: []session{
				{id: "Q8ME", state: "detached", command: []string{"python", "sensor.py"}, cwd: "/srv/sensors", createdAt: pickerTestNow.Add(-2 * time.Hour)},
			},
		},
	}
}

func key(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func runeKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Text: string(code)}
}

func updateModel(t *testing.T, current model, message tea.Msg) model {
	t.Helper()
	updated, _ := current.Update(message)
	result, ok := updated.(model)
	if !ok {
		t.Fatalf("updated model has type %T", updated)
	}
	return result
}

func assertFits(t *testing.T, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		t.Fatalf("view has %d rows, want at most %d:\n%s", len(lines), height, view)
	}
	for row, line := range lines {
		if columns := ansi.StringWidth(line); columns > width {
			t.Fatalf("view row %d has %d columns, want at most %d:\n%s", row+1, columns, width, view)
		}
	}
}

func cleanSnapshot(view string) string {
	lines := strings.Split(view, "\n")
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " ")
	}
	return strings.Join(lines, "\n")
}
