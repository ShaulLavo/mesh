package terminal

import (
	"bytes"
	"reflect"
	"testing"
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := NewScreen(20, 6)
			if _, err := original.Write([]byte(tt.output)); err != nil {
				t.Fatal(err)
			}

			restored := NewScreen(20, 6)
			if _, err := restored.Write(original.Snapshot()); err != nil {
				t.Fatal(err)
			}

			if got, want := restored.Snapshot(), original.Snapshot(); !bytes.Equal(got, want) {
				t.Fatalf("restored snapshot differs\n got: %q\nwant: %q", got, want)
			}
			assertEquivalentScreens(t, restored.(*emulatorScreen), original.(*emulatorScreen))
		})
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	screen := NewScreen(10, 3)
	if _, err := screen.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}

	snapshot := screen.Snapshot()
	if _, err := screen.Write([]byte(" second")); err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(snapshot, screen.Snapshot()) {
		t.Fatal("snapshot changed with later screen output")
	}
}

func assertEquivalentScreens(t *testing.T, got, want *emulatorScreen) {
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
	if got.cursorVisible != want.cursorVisible || got.cursorStyle != want.cursorStyle || got.cursorSteady != want.cursorSteady {
		t.Fatalf("cursor state = visible %v style %v steady %v, want visible %v style %v steady %v",
			got.cursorVisible, got.cursorStyle, got.cursorSteady,
			want.cursorVisible, want.cursorStyle, want.cursorSteady)
	}
	for y := 0; y < want.emulator.Height(); y++ {
		for x := 0; x < want.emulator.Width(); x++ {
			if !reflect.DeepEqual(got.emulator.CellAt(x, y), want.emulator.CellAt(x, y)) {
				t.Fatalf("cell (%d,%d) = %#v, want %#v", x, y, got.emulator.CellAt(x, y), want.emulator.CellAt(x, y))
			}
		}
	}
}
