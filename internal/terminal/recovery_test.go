package terminal

import (
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
