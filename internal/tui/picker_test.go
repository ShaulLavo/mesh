package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
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
			want: attachSelection{hostAlias: "pc", sessionID: "91AZ", takeOver: true},
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

func TestPickerEscapesAndBoundsRemoteCommandAndWorkingDirectory(t *testing.T) {
	malicious := "ATTACKER\tFAKE\nROW\x1b[31m\u202e" + strings.Repeat("x", 10_000)
	hosts := hostCatalog(cli.PickerInput{Hosts: []cli.HostSessions{{
		Host: cli.HostRecord{Alias: "pc"}, Sessions: []protocol.SessionInfo{{
			ID: "7K3D", State: "running", Command: []string{malicious}, Cwd: malicious,
		}},
	}}})
	filter := (sessionItem{session: hosts[0].sessions[0]}).FilterValue()
	if strings.ContainsAny(filter, "\t\n\r\x1b") || strings.ContainsRune(filter, '\u202e') || len(filter) > 1200 {
		t.Fatalf("unsafe picker filter = %q (%d bytes)", filter, len(filter))
	}
	current := newModel(hosts, pickerTestNow)
	current.showSessions()
	view := ansi.Strip(current.View().Content)
	if strings.ContainsRune(view, '\u202e') || strings.Contains(view, "\nROW") || len(view) > 10_000 {
		t.Fatalf("unsafe picker view = %q (%d bytes)", view, len(view))
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
	if plain := ansi.Strip(view); !strings.Contains(plain, "No sessions") || !strings.Contains(plain, "n new") {
		t.Fatalf("empty session view lacks actions:\n%s", plain)
	}
	assertFits(t, view, 52, 16)
}

func TestPickerListsWrapAtTheirEnds(t *testing.T) {
	t.Run("hosts", func(t *testing.T) {
		current := newModel(pickerFixture(), pickerTestNow)
		current = updateModel(t, current, key(tea.KeyUp))
		if got := current.list.SelectedItem().(hostItem).host.alias; got != "pi" {
			t.Fatalf("up from first host selected %q, want last host pi", got)
		}
		current = updateModel(t, current, key(tea.KeyDown))
		if got := current.list.SelectedItem().(hostItem).host.alias; got != "pc" {
			t.Fatalf("down from last host selected %q, want first host pc", got)
		}
	})

	t.Run("sessions across pages", func(t *testing.T) {
		sessions := make([]session, 10)
		for index := range sessions {
			sessions[index] = session{
				id:        fmt.Sprintf("S%03d", index),
				state:     "detached",
				createdAt: pickerTestNow,
			}
		}
		current := newModel([]host{{alias: "pc", sessions: sessions}}, pickerTestNow)
		current = updateModel(t, current, key(tea.KeyEnter))
		current = updateModel(t, current, key(tea.KeyUp))
		if got := current.selectedSessionID(); got != "S009" || current.list.Paginator.Page == 0 {
			t.Fatalf("up from first session selected %q on page %d, want S009 on last page", got, current.list.Paginator.Page)
		}
		current = updateModel(t, current, key(tea.KeyDown))
		if got := current.selectedSessionID(); got != "S000" || current.list.Paginator.Page != 0 {
			t.Fatalf("down from last session selected %q on page %d, want S000 on first page", got, current.list.Paginator.Page)
		}
	})
}

func TestPickerOpensRequestedHostsSessionView(t *testing.T) {
	input := cli.PickerInput{
		Hosts: []cli.HostSessions{
			{Host: cli.HostRecord{Alias: "mac"}},
			{Host: cli.HostRecord{Alias: "pc"}, Sessions: []protocol.SessionInfo{{ID: "7K3D", State: "detached"}}},
		},
		OpenHostAlias: "pc",
		Inspect: func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
			return cli.SessionInspection{ObservedAt: pickerTestNow}, nil
		},
	}

	current := newPickerModel(context.Background(), input, pickerTestNow)
	if current.screen != sessionScreen || current.currentHost().alias != "pc" {
		t.Fatalf("picker opened screen %d on host %q, want pc session screen", current.screen, current.currentHost().alias)
	}
	command := current.Init()
	if command == nil {
		t.Fatal("restored session view has no startup command")
	}
	updated, inspect := current.Update(command())
	started, ok := updated.(model)
	if !ok || inspect == nil || started.inspection.target.sessionID != "7K3D" {
		t.Fatalf("restored startup = model %T, inspect nil %t, target %q", updated, inspect == nil, started.inspection.target.sessionID)
	}
}

func TestRequestedHostStartsCatalogRefreshWhenWindowSizeArrivesBeforeInit(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "pc-id"}
	refreshCalls := 0
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts:         []cli.HostSessions{{Host: host}},
		OpenHostAlias: "pc",
		Refresh: func(context.Context, string) (cli.PickerHostSnapshot, error) {
			refreshCalls++
			return cli.PickerHostSnapshot{Sessions: cli.HostSessions{
				Host:     host,
				Sessions: []protocol.SessionInfo{{ID: "91AZ", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow}},
			}}, nil
		},
	}, pickerTestNow)
	delayedInit := current.Init()
	if delayedInit == nil {
		t.Fatal("preopened host returned no init command")
	}

	updated, windowCommand := current.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	started := updated.(model)
	if windowCommand == nil {
		t.Fatal("window-size startup returned no refresh command")
	}
	updated, _ = started.Update(catalogRefreshMessage(t, windowCommand))
	refreshed := updated.(model)
	if refreshCalls != 1 || refreshed.selectedSessionID() != "91AZ" {
		t.Fatalf("startup refresh calls = %d, selected session = %q", refreshCalls, refreshed.selectedSessionID())
	}

	updated, command := refreshed.Update(delayedInit())
	if command != nil || refreshCalls != 1 || updated.(model).selectedSessionID() != "91AZ" {
		t.Fatalf("late init restarted refresh: calls %d, command nil %t", refreshCalls, command == nil)
	}
}

func TestNonTerminalPickerReturnsWithoutStartingTea(t *testing.T) {
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()       //nolint:errcheck // test resource cleanup
	defer inputWriter.Close() //nolint:errcheck // test resource cleanup
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()       //nolint:errcheck // test resource cleanup
	defer outputWriter.Close() //nolint:errcheck // test resource cleanup

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
			route: "pc.tail.example",
			sessions: []session{
				{id: "7K3D", state: "detached", command: []string{"claude", "--dangerously-skip-permissions"}, cwd: "/home/shaul/src/mesh", createdAt: pickerTestNow.Add(-4 * time.Minute)},
				{id: "91AZ", state: "running", command: []string{"npm", "run", "dev"}, cwd: "/home/shaul/src/site", createdAt: pickerTestNow.Add(-8 * time.Minute)},
			},
		},
		{
			alias: "pi",
			route: "100.64.0.8:7337",
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

func TestPickerRefusesTheWrongActionForAState(t *testing.T) {
	t.Parallel()

	// Removing a live session would delete work someone means to come back to,
	// and killing a finished one has nothing to end. Both say so and stay put
	// rather than quitting the picker with a selection the CLI must reject.
	for _, row := range []struct {
		name  string
		key   string
		state string
	}{
		{"remove refuses a live session", "x", "running"},
		{"remove refuses a detached session", "x", "detached"},
		{"kill refuses a finished session", "k", "exited"},
	} {
		t.Run(row.name, func(t *testing.T) {
			m := newModel([]host{{alias: "pc", sessions: []session{{id: "7K3D", state: row.state}}}}, time.Now())
			m = updateModel(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
			m = updateModel(t, m, key(tea.KeyEnter))
			m = updateModel(t, m, key(rune(row.key[0])))
			if m.selection != nil {
				t.Fatalf("%s produced selection %#v, want none", row.key, m.selection)
			}
			if m.notice == "" {
				t.Fatalf("%s on a %s session said nothing", row.key, row.state)
			}
		})
	}
}
