package terminal

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

func TestPreviewCropsToLastRelevantRows(t *testing.T) {
	tests := []struct {
		name   string
		output string
		rows   int
		want   []string
	}{
		{
			name:   "last nonblank row is below cursor",
			output: "\x1b[1;1Htop\x1b[3;1Hmiddle\x1b[5;1Hbottom\x1b[2;1H",
			rows:   3,
			want:   []string{"middle", "", "bottom"},
		},
		{
			name:   "cursor is below last nonblank row",
			output: "\x1b[2;1Hcontent\x1b[6;1H",
			rows:   3,
			want:   []string{"", "", ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screen := NewScreen(20, 6)
			if _, err := screen.Write([]byte(tt.output)); err != nil {
				t.Fatal(err)
			}

			preview := screen.Preview(20, tt.rows)
			if got := preview.Lines; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("preview lines = %#v, want %#v", got, tt.want)
			}
			assertPreviewRunsMatchLines(t, preview)
		})
	}
}

func TestPreviewCropsByDisplayWidthWithoutControlSequences(t *testing.T) {
	screen := NewScreen(20, 3)
	output := "\x1b[31mA界B\x1b[0mC\x1b]8;;https://example.com\x07link\x1b]8;;\x07"
	if _, err := screen.Write([]byte(output)); err != nil {
		t.Fatal(err)
	}

	preview := screen.Preview(4, 1)
	if got, want := preview.Lines, []string{"A界B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("preview lines = %#v, want %#v", got, want)
	}
	if width := ansi.StringWidth(preview.Lines[0]); width > 4 {
		t.Fatalf("preview line width = %d, want at most 4", width)
	}
	if strings.ContainsAny(preview.Lines[0], "\x1b\x07") {
		t.Fatalf("preview exposed terminal control bytes: %q", preview.Lines[0])
	}
	assertPreviewRunsMatchLines(t, preview)
}

func TestPreviewPreservesRenderedCellPresentation(t *testing.T) {
	screen := NewScreen(20, 3)
	emulator := screen.(*emulatorScreen).emulator
	styleA := uv.Style{
		Fg:             ansi.Red,
		Bg:             ansi.IndexedColor(236),
		UnderlineColor: ansi.RGBColor{R: 1, G: 2, B: 3},
		Attrs:          uv.AttrBold | uv.AttrFaint | uv.AttrItalic,
		Underline:      uv.UnderlineSingle,
	}
	styleB := uv.Style{
		Fg:             ansi.IndexedColor(45),
		Bg:             ansi.RGBColor{R: 4, G: 5, B: 6},
		UnderlineColor: ansi.Yellow,
		Attrs:          uv.AttrReverse | uv.AttrStrikethrough,
		Underline:      uv.UnderlineDouble,
	}
	styleC := uv.Style{
		Fg:             ansi.RGBColor{R: 7, G: 8, B: 9},
		Bg:             ansi.Blue,
		UnderlineColor: ansi.IndexedColor(99),
		Underline:      uv.UnderlineCurly,
	}
	emulator.SetCell(0, 0, &uv.Cell{Content: "a", Width: 1, Style: styleA})
	emulator.SetCell(1, 0, &uv.Cell{
		Content: "a",
		Width:   1,
		Style:   styleA,
		Link:    uv.Link{URL: "https://example.com"},
	})
	emulator.SetCell(2, 0, &uv.Cell{Content: "b", Width: 1, Style: styleB})
	styleB.Attrs |= uv.AttrBlink | uv.AttrRapidBlink
	emulator.SetCell(3, 0, &uv.Cell{Content: "b", Width: 1, Style: styleB})
	emulator.SetCell(4, 0, &uv.Cell{Content: "\u754c", Width: 2, Style: styleC})

	preview := screen.Preview(20, 1)
	want := []PreviewLine{{Runs: []PreviewRun{
		{
			Text: "aa",
			Style: PreviewStyle{
				Foreground:     PreviewColor{Kind: PreviewColorBasic, Value: 1},
				Background:     PreviewColor{Kind: PreviewColorIndexed, Value: 236},
				UnderlineColor: PreviewColor{Kind: PreviewColorRGB, Value: 0x010203},
				Bold:           true,
				Faint:          true,
				Italic:         true,
				Underline:      PreviewUnderlineSingle,
			},
		},
		{
			Text: "bb",
			Style: PreviewStyle{
				Foreground:     PreviewColor{Kind: PreviewColorIndexed, Value: 45},
				Background:     PreviewColor{Kind: PreviewColorRGB, Value: 0x040506},
				UnderlineColor: PreviewColor{Kind: PreviewColorBasic, Value: 3},
				Underline:      PreviewUnderlineDouble,
				Reverse:        true,
				Strikethrough:  true,
			},
		},
		{
			Text: "\u754c",
			Style: PreviewStyle{
				Foreground:     PreviewColor{Kind: PreviewColorRGB, Value: 0x070809},
				Background:     PreviewColor{Kind: PreviewColorBasic, Value: 4},
				UnderlineColor: PreviewColor{Kind: PreviewColorIndexed, Value: 99},
				Underline:      PreviewUnderlineCurly,
			},
		},
	}}}
	if !reflect.DeepEqual(preview.StyledLines, want) {
		t.Fatalf("styled preview = %#v, want %#v", preview.StyledLines, want)
	}
	assertPreviewRunsMatchLines(t, preview)
	for _, line := range preview.StyledLines {
		for _, run := range line.Runs {
			for _, r := range run.Text {
				if unicode.IsControl(r) {
					t.Fatalf("styled preview exposed Unicode control %U in %q", r, run.Text)
				}
			}
		}
	}
}

func TestPreviewPreservesEveryUnderlinePresentation(t *testing.T) {
	screen := NewScreen(20, 3)
	emulator := screen.(*emulatorScreen).emulator
	underlines := []struct {
		cell uv.Underline
		want PreviewUnderline
	}{
		{cell: uv.UnderlineSingle, want: PreviewUnderlineSingle},
		{cell: uv.UnderlineDouble, want: PreviewUnderlineDouble},
		{cell: uv.UnderlineCurly, want: PreviewUnderlineCurly},
		{cell: uv.UnderlineDotted, want: PreviewUnderlineDotted},
		{cell: uv.UnderlineDashed, want: PreviewUnderlineDashed},
	}
	for x, underline := range underlines {
		emulator.SetCell(x, 0, &uv.Cell{
			Content: string(rune('a' + x)),
			Width:   1,
			Style:   uv.Style{Underline: underline.cell},
		})
	}

	preview := screen.Preview(20, 1)
	if got, want := len(preview.StyledLines[0].Runs), len(underlines); got != want {
		t.Fatalf("styled preview has %d runs, want %d", got, want)
	}
	for i, underline := range underlines {
		if got := preview.StyledLines[0].Runs[i].Style.Underline; got != underline.want {
			t.Fatalf("styled preview run %d underline = %d, want %d", i, got, underline.want)
		}
	}
	assertPreviewRunsMatchLines(t, preview)
}

func TestPreviewConcealsCellContentsWithoutChangingTheirWidth(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("A\x1b[1;8;48;5;22m\u754cx\x1b[0mB")); err != nil {
		t.Fatal(err)
	}

	preview := screen.Preview(20, 1)
	if got, want := preview.Lines, []string{"A   B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("preview lines = %#v, want %#v", got, want)
	}
	want := []PreviewLine{{Runs: []PreviewRun{
		{Text: "A"},
		{
			Text: "   ",
			Style: PreviewStyle{
				Background: PreviewColor{Kind: PreviewColorIndexed, Value: 22},
				Bold:       true,
			},
		},
		{Text: "B"},
	}}}
	if !reflect.DeepEqual(preview.StyledLines, want) {
		t.Fatalf("styled preview = %#v, want %#v", preview.StyledLines, want)
	}
	if strings.Contains(strings.Join(preview.Lines, ""), "\u754c") || strings.Contains(strings.Join(preview.Lines, ""), "x") {
		t.Fatalf("preview exposed concealed cell contents: %#v", preview.Lines)
	}
	assertPreviewRunsMatchLines(t, preview)
}

func TestPreviewKeepsTrailingStyledAndConcealedCellsWithinBounds(t *testing.T) {
	t.Run("meaningful trailing cells", func(t *testing.T) {
		screen := NewScreen(10, 3)
		if _, err := screen.Write([]byte("A \x1b[48;5;22m \x1b[0m \x1b[8m\u754c\x1b[0m  ")); err != nil {
			t.Fatal(err)
		}

		preview := screen.Preview(10, 1)
		if got, want := preview.Lines, []string{"A     "}; !reflect.DeepEqual(got, want) {
			t.Fatalf("preview lines = %#v, want %#v", got, want)
		}
		want := []PreviewLine{{Runs: []PreviewRun{
			{Text: "A "},
			{
				Text:  " ",
				Style: PreviewStyle{Background: PreviewColor{Kind: PreviewColorIndexed, Value: 22}},
			},
			{Text: "   "},
		}}}
		if !reflect.DeepEqual(preview.StyledLines, want) {
			t.Fatalf("styled preview = %#v, want %#v", preview.StyledLines, want)
		}
		assertPreviewRunsMatchLines(t, preview)
	})

	t.Run("aggregate byte limit", func(t *testing.T) {
		width := maxPreviewTextBytes + 10
		screen := NewScreen(width, 1)
		emulator := screen.(*emulatorScreen).emulator
		style := uv.Style{Bg: ansi.IndexedColor(22)}
		for x := range width {
			emulator.SetCell(x, 0, &uv.Cell{Content: " ", Width: 1, Style: style})
		}

		preview := screen.Preview(width, 1)
		if got, want := len(preview.Lines), 1; got != want {
			t.Fatalf("preview returned %d rows, want %d", got, want)
		}
		if got, want := len(preview.Lines[0]), maxPreviewTextBytes; got != want {
			t.Fatalf("preview returned %d trailing-space bytes, want %d", got, want)
		}
		assertPreviewRunsMatchLines(t, preview)
	})
}

func assertPreviewRunsMatchLines(t *testing.T, preview Preview) {
	t.Helper()
	if len(preview.StyledLines) != len(preview.Lines) {
		t.Fatalf("styled preview has %d lines, want %d", len(preview.StyledLines), len(preview.Lines))
	}
	for i, line := range preview.StyledLines {
		var text strings.Builder
		for _, run := range line.Runs {
			text.WriteString(run.Text)
		}
		if got, want := text.String(), preview.Lines[i]; got != want {
			t.Fatalf("styled preview line %d text = %q, want %q", i, got, want)
		}
	}
}

func TestPreviewRemovesUnicodeControlCharacters(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("safe\u009bunsafe")); err != nil {
		t.Fatal(err)
	}

	preview := screen.Preview(20, 1)
	if got, want := preview.Lines, []string{"safeunsafe"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("preview lines = %#v, want %#v", got, want)
	}
	for _, r := range preview.Lines[0] {
		if unicode.IsControl(r) {
			t.Fatalf("preview exposed Unicode control %U in %q", r, preview.Lines[0])
		}
	}
	assertPreviewRunsMatchLines(t, preview)
}

func TestPreviewBoundsAggregateBytesWhileExtractingCells(t *testing.T) {
	const previewByteLimit = maxPreviewTextBytes

	combiningCell := "é" + strings.Repeat("\u0301", previewByteLimit)
	screen := NewScreen(4, 3)
	output := "\x1b[1;1H" + combiningCell +
		"\x1b[2;1H" + combiningCell +
		"\x1b[3;1Htail"
	if _, err := screen.Write([]byte(output)); err != nil {
		t.Fatal(err)
	}

	preview := screen.Preview(4, 3)
	if got, want := len(preview.Lines), 1; got != want {
		t.Fatalf("preview returned %d lines after exhausting its byte budget, want %d", got, want)
	}
	total := 0
	for row, line := range preview.Lines {
		total += len(line)
		if !utf8.ValidString(line) {
			t.Fatalf("preview row %d is not valid UTF-8", row)
		}
		if width := ansi.StringWidth(line); width > 4 {
			t.Fatalf("preview row %d width = %d, want at most 4", row, width)
		}
	}
	if total == 0 || total > previewByteLimit {
		t.Fatalf("preview contains %d bytes, want 1..%d", total, previewByteLimit)
	}
	assertPreviewRunsMatchLines(t, preview)
}

func TestPreviewTracksOSCTitle(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("\x1b]0;zero\x07")); err != nil {
		t.Fatal(err)
	}
	if got, want := screen.Preview(20, 3).Title, "zero"; got != want {
		t.Fatalf("OSC 0 title = %q, want %q", got, want)
	}
	if _, err := screen.Write([]byte("\x1b]2;two;parts\x1b\\")); err != nil {
		t.Fatal(err)
	}

	if got, want := screen.Preview(20, 3).Title, "two;parts"; got != want {
		t.Fatalf("OSC 2 title = %q, want %q", got, want)
	}
}

func TestPreviewUsesActiveScreen(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("main\x1b[?1049h\x1b[Halt")); err != nil {
		t.Fatal(err)
	}
	if got, want := screen.Preview(20, 3).Lines, []string{"alt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("alternate-screen preview = %#v, want %#v", got, want)
	}

	if _, err := screen.Write([]byte("\x1b[?1049l")); err != nil {
		t.Fatal(err)
	}
	if got, want := screen.Preview(20, 3).Lines, []string{"main"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("main-screen preview = %#v, want %#v", got, want)
	}
}

func TestPreviewTracksAbsoluteOSC7FileDirectory(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("\x1b]7;file://mesh-host/tmp/hello%20world\x07")); err != nil {
		t.Fatal(err)
	}

	if got, want := screen.Preview(20, 3).Directory, "/tmp/hello world"; got != want {
		t.Fatalf("directory = %q, want %q", got, want)
	}
}

func TestPreviewIgnoresMalformedOSCMetadata(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("\x1b]2;good title\x07\x1b]7;file:///good/path\x07")); err != nil {
		t.Fatal(err)
	}

	invalid := []string{
		"\x1b]2x;bad title\x07",
		"\x1b]7;https://example.com/not-file\x07",
		"\x1b]7;file:relative/path\x07",
		"\x1b]7;file:///bad%zzpath\x07",
		"\x1b]7;file:///bad/path?query\x07",
		"\x1b]7;file:///bad%00path\x07",
	}
	for _, sequence := range invalid {
		if _, err := screen.Write([]byte(sequence)); err != nil {
			t.Fatal(err)
		}
	}

	preview := screen.Preview(20, 3)
	if got, want := preview.Title, "good title"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	if got, want := preview.Directory, "/good/path"; got != want {
		t.Fatalf("directory = %q, want %q", got, want)
	}
}

func TestPreviewBoundsOSCMetadata(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("\x1b]2;good title\x07\x1b]7;file:///good/path\x07")); err != nil {
		t.Fatal(err)
	}

	oversizedTitle := "\x1b]2;" + strings.Repeat("x", maxOSCMetadataBytes+1) + "\x07"
	oversizedDirectory := "\x1b]7;file:///" + strings.Repeat("x", maxOSCMetadataBytes+1) + "\x07"
	if _, err := screen.Write([]byte(oversizedTitle + oversizedDirectory)); err != nil {
		t.Fatal(err)
	}

	preview := screen.Preview(20, 3)
	if got, want := preview.Title, "good title"; got != want {
		t.Fatalf("title = %q after oversized OSC, want %q", got, want)
	}
	if got, want := preview.Directory, "/good/path"; got != want {
		t.Fatalf("directory = %q after oversized OSC, want %q", got, want)
	}
}

func TestPreviewReturnsIndependentCopies(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("first\x1b]2;title\x07\x1b]7;file:///directory\x07")); err != nil {
		t.Fatal(err)
	}

	first := screen.Preview(20, 3)
	first.Lines[0] = "mutated"
	first.StyledLines[0].Runs[0].Text = "mutated"
	second := screen.Preview(20, 3)
	if got, want := second.Lines[0], "first"; got != want {
		t.Fatalf("later preview line = %q, want %q", got, want)
	}
	if got, want := second.Title, "title"; got != want {
		t.Fatalf("later preview title = %q, want %q", got, want)
	}
	if got, want := second.Directory, "/directory"; got != want {
		t.Fatalf("later preview directory = %q, want %q", got, want)
	}
	assertPreviewRunsMatchLines(t, second)
}

func TestPreviewWithNonpositiveBoundsReturnsOnlyMetadata(t *testing.T) {
	screen := NewScreen(20, 3)
	if _, err := screen.Write([]byte("content\x1b]2;title\x07\x1b]7;file:///directory\x07")); err != nil {
		t.Fatal(err)
	}

	preview := screen.Preview(0, -1)
	if len(preview.Lines) != 0 {
		t.Fatalf("preview lines = %#v, want none", preview.Lines)
	}
	if len(preview.StyledLines) != 0 {
		t.Fatalf("styled preview lines = %#v, want none", preview.StyledLines)
	}
	if preview.Title != "title" || preview.Directory != "/directory" {
		t.Fatalf("metadata = title %q directory %q, want title and /directory", preview.Title, preview.Directory)
	}
}

func TestSnapshotRecreatesScreen(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "main screen with styles and positioned cursor",
			output: "\x1b[2J\x1b[2;3Hplain \x1b[1;31mred\x1b[0m\x1b[4;9H\x1b[4 q",
		},
		{
			name:   "alternate screen with hidden cursor",
			output: "before\x1b[?1049h\x1b[2J\x1b[3;4H\x1b[38;5;45mALT\x1b[0m\x1b[?25l",
		},
		{
			name:   "multiple populated rows",
			output: "\x1b[1;1Hfirst\x1b[2;1Hsecond\x1b[3;1Hthird",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := NewScreen(20, 6)
			if _, err := original.Write([]byte(tt.output)); err != nil {
				t.Fatal(err)
			}

			capture := original.Snapshot()
			if !capture.Restorable {
				t.Fatal("ordinary screen was not restorable")
			}
			restored := NewScreen(20, 6)
			if _, err := restored.Write(capture.Bytes); err != nil {
				t.Fatal(err)
			}

			got, want := restored.Snapshot(), original.Snapshot()
			if got.Restorable != want.Restorable || !bytes.Equal(got.Bytes, want.Bytes) {
				t.Fatalf("restored snapshot differs\n got: %q\nwant: %q", got.Bytes, want.Bytes)
			}
			assertEquivalentScreens(t, restored.(*emulatorScreen), original.(*emulatorScreen))
		})
	}
}

func TestSnapshotRestoresWindowTitle(t *testing.T) {
	const title = "shaul@omarchy:~"

	original := NewScreen(80, 24)
	if _, err := original.Write([]byte("\x1b]0;" + title + "\x07")); err != nil {
		t.Fatal(err)
	}
	restored := NewScreen(80, 24)
	if _, err := restored.Write(original.Snapshot().Bytes); err != nil {
		t.Fatal(err)
	}

	if got := restored.Preview(1, 1).Title; got != title {
		t.Fatalf("restored title = %q, want %q", got, title)
	}
}

func TestSnapshotUsesCursorStateRestoredByDECRC(t *testing.T) {
	screen := NewScreen(20, 6)
	if _, err := screen.Write([]byte("\x1b[2 q\x1b[?25l\x1b7\x1b[5 q\x1b[?25h\x1b8")); err != nil {
		t.Fatal(err)
	}

	snapshot := screen.Snapshot()
	if !bytes.Contains(snapshot.Bytes, []byte("\x1b[2 q\x1b[?25l")) {
		t.Fatalf("snapshot did not restore the saved steady block cursor: %q", snapshot.Bytes)
	}
}

func TestSnapshotUsesMainScreenCursorAfterLeavingAlternateScreen(t *testing.T) {
	screen := NewScreen(20, 6)
	if _, err := screen.Write([]byte("\x1b[2 q\x1b[?25l\x1b[?1049h\x1b[5 q\x1b[?25h\x1b[?1049l")); err != nil {
		t.Fatal(err)
	}

	snapshot := screen.Snapshot()
	if !bytes.Contains(snapshot.Bytes, []byte("\x1b[2 q\x1b[?25l")) {
		t.Fatalf("snapshot did not restore the main-screen cursor: %q", snapshot.Bytes)
	}
}

func TestSnapshotPreservesIncompleteParserSequence(t *testing.T) {
	tests := []struct {
		name   string
		prefix []byte
		suffix []byte
	}{
		{
			name:   "CSI",
			prefix: []byte("base\x1b[31"),
			suffix: []byte("mX"),
		},
		{
			name:   "OSC",
			prefix: []byte("\x1b]8;;https://example.com"),
			suffix: []byte("\x1b\\X"),
		},
		{
			name:   "UTF-8",
			prefix: append([]byte("\x1b[31m"), 0xe2, 0x82),
			suffix: []byte{0xac},
		},
		{
			name:   "UTF-8 continuation with C1 byte value",
			prefix: []byte{0xe2, 0x9b},
			suffix: []byte{0x80},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := NewScreen(20, 6)
			if _, err := original.Write(tt.prefix); err != nil {
				t.Fatal(err)
			}

			capture := original.Snapshot()
			if !capture.Restorable {
				t.Fatal("incomplete parser sequence was not restorable")
			}
			restored := NewScreen(20, 6)
			if _, err := restored.Write(capture.Bytes); err != nil {
				t.Fatal(err)
			}

			if _, err := original.Write(tt.suffix); err != nil {
				t.Fatal(err)
			}
			if _, err := restored.Write(tt.suffix); err != nil {
				t.Fatal(err)
			}
			assertEquivalentScreens(t, restored.(*emulatorScreen), original.(*emulatorScreen))
		})
	}
}

func TestSnapshotRestoresPenForFutureOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "current style",
			output: "\x1b[1;31m",
		},
		{
			name:   "style restored by DECRC",
			output: "\x1b[1;31m\x1b7\x1b[22;34m\x1b8",
		},
		{
			name:   "main-screen style after alternate screen",
			output: "\x1b[1;31m\x1b[?1049h\x1b[22;34m\x1b[?1049l",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := NewScreen(20, 6)
			if _, err := original.Write([]byte(tt.output)); err != nil {
				t.Fatal(err)
			}
			capture := original.Snapshot()
			if !capture.Restorable {
				t.Fatal("screen was not restorable")
			}
			restored := NewScreen(20, 6)
			if _, err := restored.Write(capture.Bytes); err != nil {
				t.Fatal(err)
			}

			if _, err := original.Write([]byte("X")); err != nil {
				t.Fatal(err)
			}
			if _, err := restored.Write([]byte("X")); err != nil {
				t.Fatal(err)
			}
			assertEquivalentActiveState(t, restored.(*emulatorScreen), original.(*emulatorScreen))
		})
	}
}

func TestSnapshotRestoresHyperlinkForFutureOutput(t *testing.T) {
	for _, tt := range []struct {
		name     string
		sequence string
	}{
		{
			name:     "UTF-8 URL containing a C1-valued byte",
			sequence: "\x1b]8;id=mesh;https://example.com/\xc3\x9d\x07",
		},
		{
			name:     "leading-zero command",
			sequence: "\x1b]008;id=mesh;https://example.com\x07",
		},
		{
			name:     "signed command is not numeric",
			sequence: "\x1b]+8;id=mesh;https://example.com\x07",
		},
		{
			name:     "numeric prefix command",
			sequence: "\x1b]8x;id=mesh;https://example.com\x07",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := NewScreen(20, 6)
			if _, err := original.Write([]byte(tt.sequence)); err != nil {
				t.Fatal(err)
			}
			capture := original.Snapshot()
			if !capture.Restorable {
				t.Fatal("screen was not restorable")
			}
			restored := NewScreen(20, 6)
			if _, err := restored.Write(capture.Bytes); err != nil {
				t.Fatal(err)
			}

			if _, err := original.Write([]byte("X")); err != nil {
				t.Fatal(err)
			}
			if _, err := restored.Write([]byte("X")); err != nil {
				t.Fatal(err)
			}
			assertEquivalentActiveState(t, restored.(*emulatorScreen), original.(*emulatorScreen))
		})
	}
}

func TestSnapshotClearsReceiverHyperlinkBeforeRepaint(t *testing.T) {
	original := NewScreen(20, 6)
	if _, err := original.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	capture := original.Snapshot()
	restored := NewScreen(20, 6)
	if _, err := restored.Write([]byte("\x1b]8;id=old;https://old.example\x07")); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Write(capture.Bytes); err != nil {
		t.Fatal(err)
	}

	assertEquivalentActiveState(t, restored.(*emulatorScreen), original.(*emulatorScreen))
}

func TestSnapshotRejectsIncompleteSequenceWithExecuteByte(t *testing.T) {
	screen := NewScreen(20, 6)
	if _, err := screen.Write([]byte("\x1b[31\n")); err != nil {
		t.Fatal(err)
	}

	if capture := screen.Snapshot(); capture.Restorable {
		t.Fatalf("snapshot with an executed byte reported itself restorable: %q", capture.Bytes)
	}
}

func TestSnapshotRejectsDoubleEscapeBoundary(t *testing.T) {
	screen := NewScreen(20, 6)
	if _, err := screen.Write([]byte("\x1b\x1b")); err != nil {
		t.Fatal(err)
	}

	if capture := screen.Snapshot(); capture.Restorable {
		t.Fatalf("double-escape parser boundary reported itself restorable: %q", capture.Bytes)
	}
}

func TestParserTailKeepsDCSBytesThatArePayloadInCurrentState(t *testing.T) {
	for _, tt := range []struct {
		name   string
		prefix []byte
	}{
		{
			name:   "C1-valued payload",
			prefix: []byte{ansi.ESC, 'P', 'q', ansi.CSI, ansi.OSC},
		},
		{
			name:   "ESC accepted while entering passthrough",
			prefix: []byte{ansi.ESC, 'P', ansi.ESC},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			screen := NewScreen(20, 6)
			if _, err := screen.Write(tt.prefix); err != nil {
				t.Fatal(err)
			}
			capture := screen.Snapshot()
			if !capture.Restorable {
				t.Fatal("DCS payload boundary was not restorable")
			}
			if tail := screen.(*emulatorScreen).parserTail; !bytes.Equal(tail, tt.prefix) {
				t.Fatalf("parser tail = % x, want % x", tail, tt.prefix)
			}
		})
	}
}

func TestSnapshotBoundsIncompleteParserTail(t *testing.T) {
	screen := NewScreen(20, 6)
	sequence := make([]byte, 0, maxParserTail+8)
	sequence = append(sequence, "\x1b]8;;"...)
	sequence = append(sequence, bytes.Repeat([]byte{'x'}, maxParserTail)...)
	if _, err := screen.Write(sequence); err != nil {
		t.Fatal(err)
	}

	if capture := screen.Snapshot(); capture.Restorable {
		t.Fatal("overflowed parser tail reported itself restorable")
	}
	if tail := screen.(*emulatorScreen).parserTail; len(tail) != 0 {
		t.Fatalf("overflowed parser tail retained %d bytes", len(tail))
	}

	// Completing the oversized OSC leaves its hyperlink state unknown to the
	// bounded mirror. A later valid OSC 8 command makes it exact again.
	if _, err := screen.Write([]byte("\x07")); err != nil {
		t.Fatal(err)
	}
	if capture := screen.Snapshot(); capture.Restorable {
		t.Fatal("unknown hyperlink state reported itself restorable")
	}
	if _, err := screen.Write([]byte("\x1b]8;;\x07")); err != nil {
		t.Fatal(err)
	}
	if capture := screen.Snapshot(); !capture.Restorable {
		t.Fatal("valid hyperlink reset did not restore capture support")
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	screen := NewScreen(10, 3)
	if _, err := screen.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}

	snapshot := screen.Snapshot().Bytes
	if _, err := screen.Write([]byte(" second")); err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(snapshot, screen.Snapshot().Bytes) {
		t.Fatal("snapshot changed with later screen output")
	}
}

func assertEquivalentScreens(t *testing.T, got, want *emulatorScreen) {
	t.Helper()
	assertEquivalentActiveState(t, got, want)
	if !reflect.DeepEqual(got.cursors, want.cursors) {
		t.Fatalf("cursor state = %#v, want %#v", got.cursors, want.cursors)
	}
}

func assertEquivalentActiveState(t *testing.T, got, want *emulatorScreen) {
	t.Helper()
	if got.emulator.Width() != want.emulator.Width() || got.emulator.Height() != want.emulator.Height() {
		t.Fatalf("dimensions = %dx%d, want %dx%d", got.emulator.Width(), got.emulator.Height(), want.emulator.Width(), want.emulator.Height())
	}
	if got.emulator.IsAltScreen() != want.emulator.IsAltScreen() {
		t.Fatalf("alternate screen = %v, want %v", got.emulator.IsAltScreen(), want.emulator.IsAltScreen())
	}
	if got.emulator.CursorPosition() != want.emulator.CursorPosition() {
		t.Fatalf("cursor = %v, want %v", got.emulator.CursorPosition(), want.emulator.CursorPosition())
	}
	if got.activeScreen != want.activeScreen || got.leftRightMode != want.leftRightMode || !reflect.DeepEqual(got.cursors[got.activeScreen].current, want.cursors[want.activeScreen].current) {
		t.Fatalf("active cursor state = %#v active %d margins %v, want %#v active %d margins %v",
			got.cursors[got.activeScreen].current, got.activeScreen, got.leftRightMode,
			want.cursors[want.activeScreen].current, want.activeScreen, want.leftRightMode)
	}
	if got.parser.State() != want.parser.State() || !bytes.Equal(got.parserTail, want.parserTail) || got.tailOverflow != want.tailOverflow || got.tailHasExecute != want.tailHasExecute || got.linkKnown != want.linkKnown {
		t.Fatalf("parser state = %s tail %q overflow %v execute %v link known %v, want %s tail %q overflow %v execute %v link known %v",
			got.parser.StateName(), got.parserTail, got.tailOverflow, got.tailHasExecute, got.linkKnown,
			want.parser.StateName(), want.parserTail, want.tailOverflow, want.tailHasExecute, want.linkKnown)
	}
	for y := 0; y < want.emulator.Height(); y++ {
		for x := 0; x < want.emulator.Width(); x++ {
			if !reflect.DeepEqual(got.emulator.CellAt(x, y), want.emulator.CellAt(x, y)) {
				t.Fatalf("cell (%d,%d) = %#v, want %#v", x, y, got.emulator.CellAt(x, y), want.emulator.CellAt(x, y))
			}
		}
	}
}

// x/vt replies to cursor-position and device-attribute queries by writing to an
// internal pipe. Nothing in Mesh reads it, so an unclosed pipe blocks Write
// forever — and the worker holds its mutex across that call, wedging the whole
// session with meta.json still reading "running".
func TestScreenWriteDoesNotBlockOnEmulatorReplies(t *testing.T) {
	t.Parallel()

	queries := map[string]string{
		"DSR cursor position":         "\x1b[6n",
		"DECXCPR extended position":   "\x1b[?6n",
		"primary device attributes":   "\x1b[c",
		"secondary device attributes": "\x1b[>c",
		"DSR operating status":        "\x1b[5n",
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			screen := NewScreen(80, 24)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = screen.Write([]byte("before" + query + "after"))
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("Screen.Write blocked on the emulator reply to %q", query)
			}
			if got := screen.Snapshot(); len(got.Bytes) == 0 {
				t.Fatal("snapshot is empty after a write that included a query")
			}
		})
	}
}
