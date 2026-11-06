package terminal

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	ansiparser "github.com/charmbracelet/x/ansi/parser"
)

func TestCurrentPinShortParserSplits(t *testing.T) {
	cases := []struct{ name, sequence string }{
		{"osc-bel", "\x1b]2;title\x07X"},
		{"osc-st", "\x1b]2;title\x1b\\X"},
		{"hyperlink", "\x1b]8;;https://example.test\x07X\x1b]8;;\x07"},
		{"dcs-c1-payload", "\x1bPq\x9b\x9ddata\x1b\\X"},
		{"dcs-entry-escape", "\x1bP\x1bdata\x1b\\X"},
		{"dcs-cancel", "\x1bPqdata\x18X"},
		{"osc-cancel", "\x1b]2;data\x1aX"},
		{"escape-replacement", "\x1b[12\x1b[31mR"},
		{"c1-replacement", "\x1b[12\x9b31mR"},
		{"embedded-execute", "\x1b[31\nmR"},
		{"sos", "\x1bXignored\x1b\\X"},
		{"pm", "\x1b^ignored\x1b\\X"},
		{"apc", "\x1b_ignored\x1b\\X"},
		{"utf8-wide-combining", "界e\u0301λ"},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) { checkShortParserSplits(t, scenario.sequence) })
	}
}

func checkShortParserSplits(t *testing.T, sequence string) {
	t.Helper()
	control := NewScreen(40, 4)
	writeCorpus(t, control, sequence)
	for split := 0; split <= len(sequence); split++ {
		t.Run(fmt.Sprint(split), func(t *testing.T) { checkParserSplit(t, sequence, split, control) })
	}
}

func checkParserSplit(t *testing.T, sequence string, split int, control Screen) {
	t.Helper()
	screen := NewScreen(40, 4)
	writeCorpus(t, screen, sequence[:split])
	capture := screen.Snapshot()
	writeCorpus(t, screen, sequence[split:])
	assertEquivalentActiveState(t, screen.(*emulatorScreen), control.(*emulatorScreen))
	if !reflect.DeepEqual(screen.Preview(40, 4), control.Preview(40, 4)) {
		t.Fatal("Snapshot or chunk boundary changed later preview")
	}
	if !capture.Restorable {
		return
	}
	restored := NewScreen(40, 4)
	writeCorpus(t, restored, string(capture.Bytes))
	writeCorpus(t, restored, sequence[split:])
	got, want := restored.Snapshot(), screen.Snapshot()
	if got.Restorable != want.Restorable || !bytes.Equal(got.Bytes, want.Bytes) {
		t.Fatalf("split %d snapshot continuation differs\n got %q\nwant %q", split, got.Bytes, want.Bytes)
	}
}

func TestCurrentPinLargeParserBoundaries(t *testing.T) {
	for _, ending := range []string{"\x07", "\x1b\\", "\x9c"} {
		t.Run(fmt.Sprintf("osc-%x", ending), func(t *testing.T) { checkLargeOSCSizes(t, ending) })
	}
	for _, collected := range []int{maxParserTail - 1, maxParserTail, maxParserTail + 1} {
		t.Run(fmt.Sprintf("dcs/%d", collected), func(t *testing.T) { checkLargeDCS(t, collected) })
	}
}

func checkLargeOSCSizes(t *testing.T, ending string) {
	for _, collected := range []int{maxParserTail - 1, maxParserTail, maxParserTail + 1} {
		t.Run(fmt.Sprint(collected), func(t *testing.T) { checkLargeOSC(t, ending, collected) })
	}
}

func parseChunks(parser *ansi.Parser, data []byte) {
	// Only selected boundaries for large input, not every possible split.
	offset := 0
	for _, boundary := range []int{1, 2, 3, 255, 256, 257, maxParserTail - 2, maxParserTail, maxParserTail + 1, len(data)} {
		end := min(boundary, len(data))
		if end <= offset {
			continue
		}
		parser.Parse(data[offset:end])
		offset = end
	}
}

func writeLargeChunks(t *testing.T, screen Screen, data []byte) {
	t.Helper()
	for len(data) > 0 {
		size := min(len(data), 32749)
		if n, err := screen.Write(data[:size]); n != size || err != nil {
			t.Fatalf("large Write = %d, %v", n, err)
		}
		data = data[size:]
	}
}

func checkLargeOSC(t *testing.T, ending string, collected int) {
	t.Helper()
	data := append([]byte("2;"), bytes.Repeat([]byte{'x'}, collected-2)...)
	sequence := append([]byte("\x1b]"), data...)
	parser := ansi.NewParser()
	parser.SetDataSize(maxParserTail)
	var dispatched []byte
	parser.SetHandler(ansi.Handler{HandleOsc: func(_ int, payload []byte) { dispatched = bytes.Clone(payload) }})
	parseChunks(parser, sequence)
	if len(parser.Data()) != min(collected, maxParserTail) || parser.State() != ansiparser.OscStringState {
		t.Fatal("ANSI collected-data bound/state changed before termination")
	}
	screen := NewScreen(20, 3).(*emulatorScreen)
	writeLargeChunks(t, screen, sequence[:maxParserTail])
	if screen.tailOverflow || len(screen.parserTail) != maxParserTail || !screen.Snapshot().Restorable {
		t.Fatal("Mesh tail overflowed before its own byte limit")
	}
	writeLargeChunks(t, screen, sequence[maxParserTail:])
	if !screen.tailOverflow || len(screen.parserTail) != 0 || screen.Snapshot().Restorable {
		t.Fatal("Mesh introducer overhead did not overflow before ANSI data bound")
	}
	parser.Parse([]byte(ending))
	writeCorpus(t, screen, ending)
	if !bytes.Equal(dispatched, data[:min(collected, maxParserTail)]) || parser.State() != ansiparser.GroundState {
		t.Fatal("ANSI overflow dispatch differs from truncated current-pin payload")
	}
	checkUnknownLinkAfterOverflow(t, screen)
}

func checkUnknownLinkAfterOverflow(t *testing.T, screen *emulatorScreen) {
	t.Helper()
	if screen.tailOverflow || len(screen.parserTail) != 0 || screen.parser.State() != ansiparser.GroundState {
		t.Fatal("ground state did not reset overflow/tail")
	}
	if screen.linkKnown || screen.Snapshot().Restorable {
		t.Fatal("OSC overflow did not leave hyperlink state unknown")
	}
	writeCorpus(t, screen, "\x1b]2;short\x07")
	if screen.Preview(20, 3).Title != "short" || screen.Snapshot().Restorable {
		t.Fatal("short title incorrectly repaired unknown hyperlink state")
	}
	writeCorpus(t, screen, "\x1b]8;;\x07")
	if !screen.linkKnown || !screen.Snapshot().Restorable {
		t.Fatal("complete hyperlink reset did not restore known state")
	}
	writeCorpus(t, screen, "Z")
	if got := screen.SaveText(3, 128).Render(); len(got) != 1 || got[0] != "Z" {
		t.Fatalf("print after parser recovery = %q", got)
	}
}

func checkLargeDCS(t *testing.T, collected int) {
	t.Helper()
	data := bytes.Repeat([]byte{'x'}, collected)
	sequence := append([]byte("\x1bPq"), data...)
	parser := ansi.NewParser()
	parser.SetDataSize(maxParserTail)
	var dispatched []byte
	parser.SetHandler(ansi.Handler{HandleDcs: func(_ ansi.Cmd, _ ansi.Params, payload []byte) { dispatched = bytes.Clone(payload) }})
	parseChunks(parser, sequence)
	screen := NewScreen(20, 3).(*emulatorScreen)
	writeLargeChunks(t, screen, sequence)
	if !screen.tailOverflow || screen.Snapshot().Restorable || len(parser.Data()) != min(collected, maxParserTail) {
		t.Fatal("DCS intermediate overflow/collected bound changed")
	}
	parser.Parse([]byte("\x1b\\"))
	writeCorpus(t, screen, "\x1b\\")
	if !bytes.Equal(dispatched, data[:min(collected, maxParserTail)]) {
		t.Fatal("DCS dispatch did not retain the bounded payload prefix")
	}
	if screen.tailOverflow || !screen.linkKnown || !screen.Snapshot().Restorable {
		t.Fatal("DCS termination did not reset tail without changing hyperlink state")
	}
}

func TestCurrentPinOverflowCancellationAndReset(t *testing.T) {
	for _, ending := range []string{"\x18", "\x1a", "\x1bc"} {
		t.Run(fmt.Sprintf("%x", ending), func(t *testing.T) { checkOverflowCancellation(t, ending) })
	}
}

func checkOverflowCancellation(t *testing.T, ending string) {
	t.Helper()
	screen := NewScreen(20, 3).(*emulatorScreen)
	writeCorpus(t, screen, "\x1b]8;;"+strings.Repeat("x", maxParserTail))
	if !screen.tailOverflow || screen.Snapshot().Restorable {
		t.Fatal("oversized incomplete hyperlink was restorable")
	}
	writeCorpus(t, screen, ending)
	if screen.tailOverflow || !screen.linkKnown || !screen.Snapshot().Restorable {
		t.Fatal("cancellation/reset did not leave ground with known hyperlink state")
	}
	writeCorpus(t, screen, "\x1b[31mR")
	if got := screen.SaveText(3, 128).Render(); len(got) != 1 || got[0] != "R" {
		t.Fatalf("text after cancellation/reset = %q", got)
	}
}
