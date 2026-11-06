package terminal

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSavedTextIncludesRenderedScrollbackAndHidesConcealedCells(t *testing.T) {
	screen := NewScreen(30, 3)
	_, _ = screen.Write([]byte("first\r\n\x1b[31msecond\x1b[0m\r\nold\rnew\r\n\x1b[8msecret\x1b[0mvisible"))
	snapshot := screen.SaveText(256, 128<<10)
	_, _ = screen.Write([]byte(" later"))
	text := strings.Join(snapshot.Render(), "\n")
	if !strings.Contains(text, "first\nsecond\nnew\n      visible") {
		t.Fatalf("saved rendered text = %q", text)
	}
	if strings.Contains(text, "secret") || strings.Contains(text, "\x1b") || strings.Contains(text, "later") {
		t.Fatalf("saved text leaked controls, concealment, or later mutation: %q", text)
	}
}

func TestSavedTextKeepsNewestBoundedLinesAndValidUTF8(t *testing.T) {
	screen := NewScreen(40, 3)
	for index := range 400 {
		_, _ = fmt.Fprintf(screen, "line-%03d λ\r\n", index)
	}
	lines := screen.SaveText(256, 128<<10).Render()
	if len(lines) != 256 || !strings.Contains(strings.Join(lines, "\n"), "line-399") || strings.Contains(strings.Join(lines, "\n"), "line-000") {
		t.Fatalf("retained %d lines, first %q, last %q", len(lines), lines[0], lines[len(lines)-1])
	}
	text := strings.Join(screen.SaveText(256, 43).Render(), "")
	if len(text) > 43 || !utf8.ValidString(text) {
		t.Fatalf("bounded text = %q (%d bytes)", text, len(text))
	}
}

func BenchmarkSaveTextFullScreen(b *testing.B) {
	screen := NewScreen(200, 50)
	for index := range 400 {
		_, _ = fmt.Fprintf(screen, "line-%03d %s\r\n", index, strings.Repeat("x", 150))
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = screen.SaveText(256, 128<<10).Render()
	}
}

func TestScreenRevisionAdvancesOnlyForAcceptedOutputAndResize(t *testing.T) {
	screen := NewScreen(20, 5)
	if got := screen.Revision(); got != 0 {
		t.Fatalf("new screen revision = %d, want 0", got)
	}
	operations := []struct {
		name    string
		run     func()
		advance uint64
	}{
		{"empty write", func() { writeCorpus(t, screen, "") }, 0},
		{"output", func() { writeCorpus(t, screen, "x") }, 1},
		{"title only", func() { writeCorpus(t, screen, "\x1b]2;t\x07") }, 1},
		{"partial escape", func() { writeCorpus(t, screen, "\x1b[") }, 1},
		{"snapshot with partial escape", func() { _ = screen.Snapshot() }, 0},
		{"parser cancellation", func() { writeCorpus(t, screen, "\x18") }, 1},
		{"bell", func() { writeCorpus(t, screen, "\x07") }, 1},
		{"cursor move", func() { writeCorpus(t, screen, "\x1b[H") }, 1},
		{"same size", func() { screen.Resize(20, 5) }, 1},
		{"new size", func() { screen.Resize(30, 6) }, 1},
		{"RIS", func() { writeCorpus(t, screen, "\x1bc") }, 1},
		{"alternate entry", func() { writeCorpus(t, screen, enterAltScreen) }, 1},
		{"alternate exit", func() { writeCorpus(t, screen, leaveAltScreen) }, 1},
		{"snapshot", func() { _ = screen.Snapshot() }, 0},
		{"preview", func() { _ = screen.Preview(20, 5) }, 0},
		{"saved text", func() { _ = screen.SaveText(256, 128<<10) }, 0},
		{"zero-bound saved text", func() { _ = screen.SaveText(0, 0) }, 0},
		{"revision", func() { _ = screen.Revision() }, 0},
	}
	var want uint64
	for _, operation := range operations {
		operation.run()
		want += operation.advance
		if got := screen.Revision(); got != want {
			t.Fatalf("%s: revision = %d, want %d", operation.name, got, want)
		}
		if got := screen.SaveText(256, 128<<10).Revision; got != want {
			t.Fatalf("%s: saved revision = %d, want %d", operation.name, got, want)
		}
	}
}

func TestSavedTextRevisionMatchesCopiedRows(t *testing.T) {
	const writes = 300
	screen := NewScreen(20, 5)
	assertSavedRevisionLine(t, screen.SaveText(256, 128<<10))
	start, done := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(done)
		<-start
		result <- writeRevisionLines(screen, writes)
	}()
	t.Cleanup(func() { <-done })
	close(start)
	for {
		assertSavedRevisionLine(t, screen.SaveText(256, 128<<10))
		select {
		case <-done:
			assertFinalSavedRevision(t, screen, <-result, writes)
			return
		default:
		}
	}
}

func assertFinalSavedRevision(t *testing.T, screen Screen, err error, writes uint64) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	final := screen.SaveText(256, 128<<10)
	assertSavedRevisionLine(t, final)
	if final.Revision != writes {
		t.Fatalf("final revision = %d, want %d", final.Revision, writes)
	}
}

func writeRevisionLines(screen Screen, count int) error {
	for index := range count {
		line := fmt.Sprintf("L%04d\r\n", index)
		n, err := screen.Write([]byte(line))
		if err != nil {
			return fmt.Errorf("line %d: %w", index, err)
		}
		if n != len(line) {
			return fmt.Errorf("line %d: Write accepted %d bytes, want %d", index, n, len(line))
		}
	}
	return nil
}

func assertSavedRevisionLine(t *testing.T, snapshot TextSnapshot) {
	t.Helper()
	text := strings.TrimRight(strings.Join(snapshot.Render(), "\n"), "\n")
	if snapshot.Revision == 0 {
		if text != "" {
			t.Fatalf("revision 0 has text %q", text)
		}
		return
	}
	lines := strings.Split(text, "\n")
	want := fmt.Sprintf("L%04d", snapshot.Revision-1)
	if got := lines[len(lines)-1]; got != want {
		t.Fatalf("revision %d: last line = %q, want %q", snapshot.Revision, got, want)
	}
}

func TestCorpusStepsAdvanceRevision(t *testing.T) {
	for _, scenario := range terminalCorpus() {
		t.Run(scenario.name, func(t *testing.T) { checkCorpusStepRevisions(t, scenario) })
	}
}

func checkCorpusStepRevisions(t *testing.T, scenario terminalCase) {
	t.Helper()
	screen := NewScreen(12, 5)
	writeCorpus(t, screen, corpusSeed())
	want := uint64(1)
	old := observeTerminal(screen)
	frozen := encodeTerminal(t, old)
	metadata := screen.SaveText(0, 0)
	for _, step := range scenario.steps {
		want = checkCorpusStepRevision(t, screen, step, want)
	}
	_ = observeTerminal(screen)
	if got := screen.Revision(); got != want {
		t.Fatalf("reads changed revision to %d, want %d", got, want)
	}
	writeCorpus(t, screen, "\x1bc\x1b]2;new\x07\x1b]7;file://localhost/new\x07changed")
	screen.Resize(7, 3)
	if got := screen.Revision(); got != want+2 {
		t.Fatalf("output and resize revision = %d, want %d", got, want+2)
	}
	if !bytes.Equal(encodeTerminal(t, old), frozen) || old.saved.Revision != 1 {
		t.Fatal("old capture changed after output or resize")
	}
	assertZeroBoundMetadata(t, metadata, 1, "corpus", "/work/corpus")
	assertZeroBoundMetadata(t, screen.SaveText(0, 0), want+2, "new", "/new")
}

func checkCorpusStepRevision(t *testing.T, screen Screen, step corpusStep, want uint64) uint64 {
	t.Helper()
	if step.cols != 0 {
		screen.Resize(step.cols, step.rows)
		want++
	}
	if got := screen.Revision(); got != want {
		t.Fatalf("resize revision = %d, want %d", got, want)
	}
	writeCorpus(t, screen, step.output)
	if step.output != "" {
		want++
	}
	if got := screen.Revision(); got != want {
		t.Fatalf("write %q: revision = %d, want %d", step.output, got, want)
	}
	observed := observeTerminal(screen)
	if screen.Revision() != want || observed.saved.Revision != want {
		t.Fatalf("read revision = %d, saved = %d, want %d", screen.Revision(), observed.saved.Revision, want)
	}
	return want
}

func TestSavedTextZeroBoundsKeepRevisionAndMetadata(t *testing.T) {
	screen := NewScreen(20, 5)
	writeCorpus(t, screen, "text\x1b]2;title\x07\x1b]7;file://localhost/work/old\x07")
	for _, bounds := range [][2]int{{0, 4096}, {8, 0}, {-1, 4096}, {8, -1}, {0, 0}} {
		snapshot := screen.SaveText(bounds[0], bounds[1])
		assertZeroBoundMetadata(t, snapshot, 1, "title", "/work/old")
	}
	if got := screen.Revision(); got != 1 {
		t.Fatalf("zero-bound reads changed revision to %d", got)
	}
}

func assertZeroBoundMetadata(t *testing.T, snapshot TextSnapshot, revision uint64, title, directory string) {
	t.Helper()
	if snapshot.Revision != revision || snapshot.Title != title || snapshot.Directory != directory {
		t.Fatalf("metadata = %#v, want revision %d, title %q, directory %q", snapshot, revision, title, directory)
	}
	if len(snapshot.rows) != 0 || len(snapshot.Render()) != 0 {
		t.Fatalf("zero-bound snapshot has rows: %#v", snapshot.rows)
	}
}
