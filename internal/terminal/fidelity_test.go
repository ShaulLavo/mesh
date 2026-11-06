package terminal

import (
	"strings"
	"testing"
)

type fidelityLimitCase struct {
	name     string
	row      string
	sequence string
	absent   []string
}

// These are current-pin observations of completed sequences, not restoration
// guarantees or client-effect tests. Partial parser tails and raw offset replay
// can retain original query/OSC bytes; no real clipboard is involved here.
func TestSnapshotFidelityLimits(t *testing.T) {
	cases := []fidelityLimitCase{
		{name: "application-cursor", row: "F12", sequence: "\x1b[?1h"},
		{name: "application-keypad", row: "F12", sequence: "\x1b="},
		{name: "mouse-1000", row: "F12", sequence: "\x1b[?1000h"},
		{name: "mouse-1002", row: "F12", sequence: "\x1b[?1002h"},
		{name: "mouse-1003", row: "F12", sequence: "\x1b[?1003h"},
		{name: "mouse-sgr-1006", row: "F12", sequence: "\x1b[?1006h"},
		{name: "focus-events", row: "F12", sequence: "\x1b[?1004h"},
		{name: "bracketed-paste", row: "F12", sequence: "\x1b[?2004h"},
		{name: "autowrap-off", row: "F13", sequence: "\x1b[?7l"},
		{name: "origin", row: "F13", sequence: "\x1b[?6h"},
		{name: "insert", row: "F13", sequence: "\x1b[4h"},
		{name: "synchronized-output", row: "F13", sequence: "\x1b[?2026h"},
		{name: "cursor-position-query", row: "F15", sequence: "\x1b[6n"},
		{name: "primary-attributes-query", row: "F15", sequence: "\x1b[c"},
		{name: "secondary-attributes-query", row: "F15", sequence: "\x1b[>c"},
		{name: "bracketed-paste-query", row: "F15", sequence: "\x1b[?2004$p"},
		{name: "terminal-version-query", row: "F15", sequence: "\x1b[>q"},
		{name: "foreground-query", row: "F15", sequence: "\x1b]10;?\x07"},
		{name: "background-query", row: "F15", sequence: "\x1b]11;?\x1b\\"},
		{
			name: "clipboard-bel", row: "F18", sequence: "\x1b]52;c;bWVzaC1vc2M1Mi1jb3JwdXM=\x07",
			absent: []string{"]52;", "bWVzaC1vc2M1Mi1jb3JwdXM=", "mesh-osc52-corpus"},
		},
		{
			name: "clipboard-st", row: "F18", sequence: "\x1b]52;c;bWVzaC1vc2M1Mi1jb3JwdXM=\x1b\\",
			absent: []string{"]52;", "bWVzaC1vc2M1Mi1jb3JwdXM=", "mesh-osc52-corpus"},
		},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) { checkSnapshotFidelityLimit(t, scenario) })
	}
}

func checkSnapshotFidelityLimit(t *testing.T, scenario fidelityLimitCase) {
	t.Helper()
	const marker = "fidelity-ready"
	screen := NewScreen(40, 4)
	writeCorpus(t, screen, scenario.sequence+marker)
	capture := screen.Snapshot()
	if !capture.Restorable {
		t.Fatalf("%s: completed %s capture is not restorable", scenario.row, scenario.name)
	}
	views := []struct{ name, text string }{
		{"Snapshot", string(capture.Bytes)},
		{"Preview", strings.Join(screen.Preview(40, 4).Lines, "\n")},
		{"SaveText", strings.Join(screen.SaveText(4, 4096).Render(), "\n")},
	}
	for _, view := range views {
		if !strings.Contains(view.text, marker) {
			t.Fatalf("%s: %s lost visible completion marker: %q", scenario.row, view.name, view.text)
		}
		checkFidelitySequenceAbsent(t, scenario, view.name, view.text)
	}
}

func checkFidelitySequenceAbsent(t *testing.T, scenario fidelityLimitCase, view, text string) {
	t.Helper()
	if strings.Contains(text, scenario.sequence) {
		t.Fatalf("%s: %s now contains completed %s %q; update Plan 11 row %s",
			scenario.row, view, scenario.name, scenario.sequence, scenario.row)
	}
	for _, token := range scenario.absent {
		if strings.Contains(text, token) {
			t.Fatalf("%s: %s now contains %q; update Plan 11 row %s", scenario.row, view, token, scenario.row)
		}
	}
}
