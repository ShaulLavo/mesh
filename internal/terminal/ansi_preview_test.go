package terminal

import (
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func TestPreviewANSIOutputReplaysRawTerminalStylesWithinRequestedViewport(t *testing.T) {
	preview, err := PreviewANSIOutput(
		[]byte("plain \x1b[1;31mred\x1b[0m\r\n\x1b[48;5;22mwide\u754c\x1b[0m"),
		12,
		2,
	)
	if err != nil {
		t.Fatal(err)
	}

	wantLines := []string{"plain red", "wide\u754c"}
	if !reflect.DeepEqual(preview.Lines, wantLines) {
		t.Fatalf("preview lines = %#v, want %#v", preview.Lines, wantLines)
	}
	wantStyled := []PreviewLine{
		{Runs: []PreviewRun{
			{Text: "plain "},
			{
				Text: "red",
				Style: PreviewStyle{
					Foreground: PreviewColor{Kind: PreviewColorBasic, Value: 1},
					Bold:       true,
				},
			},
		}},
		{Runs: []PreviewRun{{
			Text:  "wide\u754c",
			Style: PreviewStyle{Background: PreviewColor{Kind: PreviewColorIndexed, Value: 22}},
		}}},
	}
	if !reflect.DeepEqual(preview.StyledLines, wantStyled) {
		t.Fatalf("styled preview = %#v, want %#v", preview.StyledLines, wantStyled)
	}
	assertPreviewRunsMatchLines(t, preview)
	for row, line := range preview.Lines {
		if width := ansi.StringWidth(line); width > 12 {
			t.Fatalf("preview row %d width = %d, want at most 12", row, width)
		}
	}
}

func TestPreviewANSIOutputDoesNotInventPresentationForPlainLogs(t *testing.T) {
	const output = "func main() {\r\n  echo '+ unchanged'\r\n}"
	preview, err := PreviewANSIOutput([]byte(output), 40, 3)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := preview.Lines, []string{"func main() {", "  echo '+ unchanged'", "}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("preview lines = %#v, want %#v", got, want)
	}
	for row, line := range preview.StyledLines {
		for _, run := range line.Runs {
			if run.Style != (PreviewStyle{}) {
				t.Fatalf("plain log row %d gained invented presentation: %#v", row, run)
			}
		}
	}
	assertPreviewRunsMatchLines(t, preview)
}

func TestPreviewANSIOutputConsumesControlsAndConcealsTextWithoutChangingWidth(t *testing.T) {
	output := []byte("A\x1b[8;48;5;22m\u754cx\x1b[0mB\x1b]8;;https://evil.invalid\x07link\x1b]8;;\x07")
	preview, err := PreviewANSIOutput(output, 20, 1)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := preview.Lines, []string{"A   Blink"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("preview lines = %#v, want %#v", got, want)
	}
	assertPreviewRunsMatchLines(t, preview)
	for row, line := range preview.StyledLines {
		for _, run := range line.Runs {
			if strings.Contains(run.Text, "evil.invalid") {
				t.Fatalf("preview row %d exposed hyperlink metadata: %#v", row, run)
			}
			for _, character := range run.Text {
				if unicode.IsControl(character) {
					t.Fatalf("preview row %d exposed terminal control %U in %#v", row, character, run)
				}
			}
		}
	}
}

func TestPreviewANSIOutputRejectsNonpositiveViewport(t *testing.T) {
	for _, dimensions := range [][2]int{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		if _, err := PreviewANSIOutput(nil, dimensions[0], dimensions[1]); err == nil {
			t.Fatalf("PreviewANSIOutput(%d, %d) error = nil", dimensions[0], dimensions[1])
		}
	}
}

func TestMatchPreviewStylesRequiresTheCompleteAuthoritativeScreen(t *testing.T) {
	replayed, err := PreviewANSIOutput([]byte("plain \x1b[32mgreen\x1b[0m\r\nnext"), 20, 2)
	if err != nil {
		t.Fatal(err)
	}

	matched, ok := MatchPreviewStyles(replayed, []string{"plain green", "next"})
	if !ok || len(matched) != 2 {
		t.Fatalf("matched styles = %#v, ok %t", matched, ok)
	}
	if got := previewLineTextForTest(matched[0]); got != "plain green" {
		t.Fatalf("matched text = %q", got)
	}
	foundGreen := false
	for _, run := range matched[0].Runs {
		if run.Text == "green" && run.Style.Foreground == (PreviewColor{Kind: PreviewColorBasic, Value: 2}) {
			foundGreen = true
		}
	}
	if !foundGreen {
		t.Fatalf("green source presentation was not retained: %#v", matched[0])
	}

	if styles, ok := MatchPreviewStyles(replayed, []string{"plain green", "different"}); ok || styles != nil {
		t.Fatalf("mismatched authoritative screen accepted: %#v", styles)
	}
}

func TestMatchPreviewStylesTrimsOnlyEmulatorBlankCellsAndNeverInventsColor(t *testing.T) {
	replayed, err := PreviewANSIOutput([]byte("\x1b[44mcolored\x1b[K\x1b[0m"), 12, 1)
	if err != nil {
		t.Fatal(err)
	}
	matched, ok := MatchPreviewStyles(replayed, []string{"colored"})
	if !ok {
		t.Fatalf("trimmed styles = %#v, ok false", matched)
	}
	if got := previewLineTextForTest(matched[0]); got != "colored" {
		t.Fatalf("trimmed text = %q, want colored", got)
	}

	plain, err := PreviewANSIOutput([]byte("func main() {}"), 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if styles, ok := MatchPreviewStyles(plain, []string{"func main() {}"}); ok || styles != nil {
		t.Fatalf("plain source gained presentation: %#v", styles)
	}
}

func previewLineTextForTest(line PreviewLine) string {
	var text strings.Builder
	for _, run := range line.Runs {
		text.WriteString(run.Text)
	}
	return text.String()
}
