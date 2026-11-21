package update

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundedProblemStripsControlCharacters(t *testing.T) {
	got := boundedProblem("line one\nline two\ttab\x1b[31mred\x00nul\u0085next\rend")
	want := "line one line two tab [31mred nul next end"
	if got != want {
		t.Fatalf("boundedProblem kept control characters: %q", got)
	}
	if plainProblemRune('é') != 'é' || plainProblemRune('\x07') != ' ' {
		t.Fatal("plainProblemRune changed printable text or kept a control character")
	}
}

func TestBoundedProblemTruncatesToValidUTF8(t *testing.T) {
	got := boundedProblem(strings.Repeat("a", 511) + "é" + strings.Repeat("b", 100))
	if len(got) > 512 || !utf8.ValidString(got) || got != strings.Repeat("a", 511) {
		t.Fatalf("boundedProblem split a rune or exceeded its bound: %d bytes %q", len(got), got[500:])
	}
}
