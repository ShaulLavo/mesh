package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/terminal"
)

func TestTerminalPickerRendersSelectsAndResizes(t *testing.T) {
	input, send := terminalPickerPipe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	changes := make(chan terminal.Size, 1)
	changes <- terminal.Size{Cols: 104, Rows: 31}
	updates := make(chan terminal.Size, 4)
	output := newTriggerWriter("unused trigger", func() {})
	initial := terminalPickerProbe{model: newModel(pickerFixture(), pickerTestNow), updates: updates}
	result := make(chan terminalPickerResult, 1)
	go func() {
		final, err := runTerminalPicker(ctx, initial, input, output, "xterm-256color", terminal.Size{Cols: 80, Rows: 24}, changes)
		result <- terminalPickerResult{model: final, err: err}
	}()
	assertTerminalPickerSize(t, ctx, updates, terminal.Size{Cols: 80, Rows: 24})
	assertTerminalPickerSize(t, ctx, updates, terminal.Size{Cols: 104, Rows: 31})
	if _, err := io.WriteString(send, "\r\r"); err != nil {
		t.Fatal(err)
	}
	finished := awaitTerminalPicker(t, ctx, result)
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	current := finished.model.(terminalPickerProbe).model
	if current.selection != (attachSelection{hostAlias: "pc", sessionID: "7K3D"}) {
		t.Fatalf("selection = %#v", current.selection)
	}
	if current.width != 104 || current.height != 31 {
		t.Fatalf("final picker size = %dx%d", current.width, current.height)
	}
	if !strings.Contains(output.String(), "pc") || !strings.Contains(output.String(), "7K3D") {
		t.Fatalf("picker did not render its host and session: %q", output.String())
	}
	_, _ = io.WriteString(output, attachStarted)
	assertRestoredBeforeAttach(t, output.String())
}

func TestTerminalPickerReleasesPersistentInputAfterSelectionAndCancellation(t *testing.T) {
	input, send := terminalPickerPipe(t)
	for cycle := range 20 {
		runReusableTerminalPicker(t, input, send, cycle%2 != 0)
		assertTerminalPickerInputReleased(t, input, send, cycle)
	}
}

func runReusableTerminalPicker(t *testing.T, input, send *os.File, canceled bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output := newTriggerWriter(enterAltScreen, func() {
		if canceled {
			cancel()
			return
		}
		_, _ = io.WriteString(send, "\rn")
	})
	size := func() terminal.Size { return terminal.Size{Cols: 80, Rows: 24} }
	picker := NewTerminalPicker(input, output, "xterm-256color", size, nil)
	selected, err := picker(ctx, cli.PickerInput{Hosts: []cli.HostSessions{{
		Host: cli.HostRecord{Alias: "pc"},
	}}})
	if canceled {
		if !errors.Is(err, tea.ErrProgramKilled) {
			t.Fatalf("canceled picker error = %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if selected.HostAlias != "pc" || !selected.New {
		t.Fatalf("selection = %#v, want a new session on pc", selected)
	}
	_, _ = io.WriteString(output, attachStarted)
	assertRestoredBeforeAttach(t, output.String())
}

func assertTerminalPickerInputReleased(t *testing.T, input, send *os.File, cycle int) {
	t.Helper()
	marker := fmt.Sprintf("attachment-input-%d", cycle)
	if _, err := io.WriteString(send, marker); err != nil {
		t.Fatalf("persistent pipe was closed: %v", err)
	}
	if err := input.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len(marker))
	if _, err := io.ReadFull(input, data); err != nil {
		t.Fatalf("following attachment lost input after cycle %d: %v", cycle, err)
	}
	if string(data) != marker {
		t.Fatalf("following attachment input = %q, want %q", data, marker)
	}
	if err := input.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func terminalPickerPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	input, send, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		_ = send.Close()
	})
	return input, send
}

type terminalPickerProbe struct {
	model
	updates chan<- terminal.Size
}

func (p terminalPickerProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	updated, command := p.model.Update(message)
	p.model = updated.(model)
	if _, ok := message.(tea.WindowSizeMsg); ok {
		p.updates <- terminal.Size{Cols: p.width, Rows: p.height}
	}
	return p, command
}

type terminalPickerResult struct {
	model tea.Model
	err   error
}

func assertTerminalPickerSize(t *testing.T, ctx context.Context, updates <-chan terminal.Size, expected terminal.Size) {
	t.Helper()
	select {
	case actual := <-updates:
		if actual != expected {
			t.Fatalf("picker size = %#v, want %#v", actual, expected)
		}
	case <-ctx.Done():
		t.Fatalf("picker did not handle size %#v", expected)
	}
}

func awaitTerminalPicker(t *testing.T, ctx context.Context, results <-chan terminalPickerResult) terminalPickerResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-ctx.Done():
		t.Fatal("terminal picker did not return")
		return terminalPickerResult{}
	}
}
