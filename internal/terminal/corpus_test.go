package terminal

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

var updateTerminalCorpus = flag.Bool("update-terminal-corpus", false, "capture current-pin terminal goldens for review")

type corpusStep struct {
	output string
	cols   int
	rows   int
}

type terminalCase struct {
	name  string
	steps []corpusStep
}

type terminalObservation struct {
	capture      Capture
	preview      Preview
	saved        TextSnapshot
	continuation *corpusContinuation
}

type corpusContinuation struct {
	SourceSnapshot string
	ReplaySnapshot string
	SourceLines    []string
	ReplayLines    []string
}

func observeTerminal(screen Screen) terminalObservation {
	return terminalObservation{capture: screen.Snapshot(), preview: screen.Preview(40, 8), saved: screen.SaveText(8, 4096)}
}

func encodeTerminal(t *testing.T, observed terminalObservation) []byte {
	t.Helper()
	value := struct {
		Snapshot     string
		Restorable   bool
		Preview      Preview
		SavedRows    []string
		SavedText    []string
		Continuation *corpusContinuation
	}{string(observed.capture.Bytes), observed.capture.Restorable, observed.preview, observed.saved.rows, observed.saved.Render(), observed.continuation}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func corpusSeed() string {
	return "\x1b]2;corpus\x07\x1b]7;file://localhost/work/corpus\x07" +
		"\x1b[1;1Hone \x1b[31mred\x1b[0m" +
		"\x1b[2;1Htwo 界e\u0301" +
		"\x1b[3;1Htri \x1b[44m  \x1b[0mZ" +
		"\x1b[4;1Hfour \x1b[1mB\x1b[0m" +
		"\x1b[5;1Hfive λ"
}

func terminalCorpus() []terminalCase {
	cases := []terminalCase{
		{name: "styled-wide-combining"},
		// This combined mouse-mode stream is a stimulus, not simultaneous mode state.
		{name: "input-modes", steps: []corpusStep{{output: "\x1b[?1h\x1b=\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h\x1b[?1004h\x1b[?2004h\x1b[?7l\x1b[?6h\x1b[4h\x1b[?2026h"}}},
		{name: "clipboard-osc52", steps: []corpusStep{{output: "\x1b]52;c;bWVzaC1vc2M1Mi1jb3JwdXM=\x07\x1b]52;c;bWVzaC1vc2M1Mi1jb3JwdXM=\x1b\\"}}},
		{name: "grapheme-clusters", steps: []corpusStep{{output: "\x1b[1;1H👨‍👩‍👧 👍🏽\x1b[2;1H🇮🇱 ❤️\x1b[3;1H\u1100\u1161"}}},
		{name: "detached-queries", steps: []corpusStep{{output: "\x1b[6n\x1b[c\x1b[>c\x1b[?2004$p\x1b[>q\x1b]10;?\x07\x1b]11;?\x1b\\"}}},
		{name: "vertical-margin-scroll", steps: []corpusStep{{output: "\x1b[2;4r\x1b[4;1H\r\nN界\x1b[2S\x1b[T"}}},
		{name: "horizontal-margin-scroll", steps: []corpusStep{{output: "\x1b[?69h\x1b[2;11s\x1b[2;4r\x1b[3;2H\x1b[2M\x1b[?69l"}}},
		{name: "outside-margin-delete", steps: []corpusStep{{output: "\x1b[2;4r\x1b[1;1H\x1b[99M"}}},
		{name: "reverse-index", steps: []corpusStep{{output: "\x1b[2;4r\x1b[2;1H\x1bM"}}},
		{name: "styled-blank-fill", steps: []corpusStep{{output: "\x1b[42m\x1b[2;1H\x1b[2M"}}},
		{name: "resize-shrink", steps: []corpusStep{{cols: 8, rows: 3}}},
		{name: "resize-shrink-grow", steps: []corpusStep{{cols: 8, rows: 3}, {cols: 16, rows: 6}, {output: "\x1b[5;2Hnew界e\u0301"}}},
		{name: "history-eviction", steps: []corpusStep{{output: corpusHistory(270)}}},
	}
	for _, count := range []string{"", "0", "1", "2", "99"} {
		name := count
		if name == "" {
			name = "default"
		}
		cases = append(cases, terminalCase{name: "delete-count-" + name, steps: []corpusStep{{output: "\x1b[2;1H\x1b[" + count + "M"}}})
	}
	for _, mode := range []string{"1047", "1049"} {
		entry := "\x1b[?" + mode + "h"
		exit := "\x1b[?" + mode + "l"
		active := entry + "ALT界\r\n\x1b[35me\u0301\x1b[0m\r\na\r\nb\r\nc\r\nd"
		cases = append(cases, terminalCase{name: "alternate-" + mode, steps: []corpusStep{{output: active}}})
		cases = append(cases, terminalCase{name: "alternate-exit-" + mode, steps: []corpusStep{{output: active + exit + entry + "again" + exit}}})
	}
	return cases
}

func corpusHistory(count int) string {
	var output strings.Builder
	for index := range count {
		fmt.Fprintf(&output, "\r\n%03d \x1b[32m界\x1b[0m e\u0301", index)
	}
	return output.String()
}

func writeCorpus(t *testing.T, screen Screen, output string) {
	t.Helper()
	if n, err := screen.Write([]byte(output)); err != nil || n != len(output) {
		t.Fatalf("Write = %d, %v, want %d bytes", n, err, len(output))
	}
}

func TestCurrentPinTerminalCorpus(t *testing.T) {
	for _, scenario := range terminalCorpus() {
		t.Run(scenario.name, func(t *testing.T) { checkTerminalCorpus(t, scenario) })
	}
}

func checkTerminalCorpus(t *testing.T, scenario terminalCase) {
	t.Helper()
	screen := NewScreen(12, 5)
	writeCorpus(t, screen, corpusSeed())
	for _, step := range scenario.steps {
		if step.cols != 0 {
			screen.Resize(step.cols, step.rows)
		}
		writeCorpus(t, screen, step.output)
	}
	observed := observeTerminal(screen)
	observed.continuation = currentPinContinuation(t, screen, observed.capture)
	frozen := encodeTerminal(t, observed)
	root, err := os.OpenRoot("testdata/terminal-corpus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	filename := scenario.name + ".json"
	if *updateTerminalCorpus {
		if err := root.WriteFile(filename, frozen, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := root.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frozen, golden) {
		t.Fatalf("current-pin output differs from %s\n%s", filename, frozen)
	}
	writeCorpus(t, screen, "\x1bc")
	writeCorpus(t, screen, corpusHistory(520))
	screen.Resize(7, 3)
	writeCorpus(t, screen, "\x1b[2J\x1b[Hmutated")
	screen.Resize(18, 6)
	if !bytes.Equal(encodeTerminal(t, observed), frozen) {
		t.Fatal("old Snapshot, Preview or SaveText changed after writes, eviction and resize")
	}
	if screen.(*emulatorScreen).emulator.ScrollbackLen() != 256 {
		t.Fatal("history did not remain bounded at 256 rows")
	}
}

func currentPinContinuation(t *testing.T, screen Screen, capture Capture) *corpusContinuation {
	t.Helper()
	if !capture.Restorable {
		t.Fatal("ordinary corpus capture is not restorable")
	}
	original := screen.(*emulatorScreen)
	restored := NewScreen(original.emulator.Width(), original.emulator.Height())
	writeCorpus(t, restored, string(capture.Bytes))
	// Record both current-pin results. Snapshot paints the active display, not
	// scrollback/margins/directory; the pin also drops isolated combining cells.
	continuation := "\x1b[1;2HZ界\x1b[0m"
	writeCorpus(t, screen, continuation)
	writeCorpus(t, restored, continuation)
	return &corpusContinuation{
		SourceSnapshot: string(screen.Snapshot().Bytes),
		ReplaySnapshot: string(restored.Snapshot().Bytes),
		SourceLines:    screen.Preview(40, 8).Lines,
		ReplayLines:    restored.Preview(40, 8).Lines,
	}
}
