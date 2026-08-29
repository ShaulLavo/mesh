package terminal

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

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
