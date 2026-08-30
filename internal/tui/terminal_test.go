package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
)

const (
	enterAltScreen = "\x1b[?1049h"
	exitAltScreen  = "\x1b[?1049l"
	attachStarted  = "<raw-attach-started>"
)

func TestProgramRestoresTerminalBeforeSelectionReturns(t *testing.T) {
	input, send := io.Pipe()
	output := newTriggerWriter(enterAltScreen, func() {
		_, _ = io.WriteString(send, "\r\r")
		_ = send.Close()
	})

	final, err := runProgram(context.Background(), newModel(pickerFixture(), pickerTestNow), input, output)
	if err != nil {
		t.Fatal(err)
	}
	finished, ok := final.(model)
	if !ok {
		t.Fatalf("final model has type %T", final)
	}
	if finished.selection != (attachSelection{hostAlias: "pc", sessionID: "7K3D"}) {
		t.Fatalf("selection = %#v", finished.selection)
	}
	_, _ = io.WriteString(output, attachStarted)
	assertRestoredBeforeAttach(t, output.String())
}

func TestProgramRestoresTerminalBeforeReturningAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input, send := io.Pipe()
	output := newTriggerWriter(enterAltScreen, cancel)

	_, err := runProgram(ctx, newModel(pickerFixture(), pickerTestNow), input, output)
	_ = send.Close()
	_ = input.Close()
	if err == nil {
		t.Fatal("runProgram returned nil error after context cancellation")
	}
	_, _ = io.WriteString(output, attachStarted)
	assertRestoredBeforeAttach(t, output.String())
}

func assertRestoredBeforeAttach(t *testing.T, output string) {
	t.Helper()
	entered := strings.Index(output, enterAltScreen)
	restored := strings.LastIndex(output, exitAltScreen)
	attached := strings.LastIndex(output, attachStarted)
	if entered < 0 || restored < entered || attached < restored {
		t.Fatalf("terminal sequence order is enter=%d restore=%d attach=%d in %q", entered, restored, attached, output)
	}
}

type triggerWriter struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	trigger []byte
	action  func()
	once    sync.Once
}

func newTriggerWriter(trigger string, action func()) *triggerWriter {
	return &triggerWriter{trigger: []byte(trigger), action: action}
}

func (w *triggerWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buffer.Write(payload)
	matched := bytes.Contains(w.buffer.Bytes(), w.trigger)
	w.mu.Unlock()
	if matched {
		w.once.Do(func() { go w.action() })
	}
	return n, err
}

func (w *triggerWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}
