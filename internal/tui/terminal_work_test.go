package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Count affected cells, including erase/repeat work hidden by stripping ANSI.
func dashboardTerminalAffectedCells(t *testing.T, emulator *vt.Emulator, output string, width, height int) int {
	t.Helper()
	parser := ansi.NewParser()
	var state byte
	cells, lastWidth := 0, 1
	for len(output) > 0 {
		sequence, printed, size, next := ansi.DecodeSequence(output, state, parser)
		state, output = next, output[size:]
		position := emulator.CursorPosition()
		if printed > 0 {
			cells += printed
			lastWidth = printed
		}
		if ansi.HasCsiPrefix(sequence) {
			parameter, _ := parser.Param(0, 1)
			command := ansi.Cmd(parser.Command()).Final()
			switch command {
			case 'b':
				cells += max(1, parameter) * lastWidth
			case 'X':
				cells += min(max(1, parameter), width-position.X)
			case 'K':
				mode, _ := parser.Param(0, 0)
				cells += dashboardTerminalEraseCells(mode, position.X, width)
			case 'J':
				mode, _ := parser.Param(0, 0)
				cells += dashboardTerminalEraseCells(mode, position.Y*width+position.X, width*height)
			case '@', 'P':
				cells += width - position.X
			case 'L', 'M', 'S', 'T':
				cells += width * height
			}
		}
		if (sequence == "\n" && position.Y == height-1) || (sequence == "\x1bM" && position.Y == 0) {
			cells += width * height
		}
		if _, err := emulator.Write([]byte(sequence)); err != nil {
			t.Fatal(err)
		}
	}
	return cells
}

func dashboardTerminalEraseCells(mode, offset, size int) int {
	switch mode {
	case 0:
		return size - offset
	case 1:
		return offset + 1
	case 2:
		return size
	default:
		return 0
	}
}

func TestDashboardTerminalAffectedCells(t *testing.T) {
	for _, example := range []struct {
		name, output string
		cells        int
	}{
		{"print", "abc", 3},
		{"repeat", "x\x1b[61b", 62},
		{"erase-characters", "\x1b[4G\x1b[5X", 5},
		{"erase-line", "\x1b[4G\x1b[K", 77},
		{"erase-display", "\x1b[2J", 160},
	} {
		t.Run(example.name, func(t *testing.T) {
			emulator := vt.NewEmulator(80, 2)
			defer func() {
				if err := emulator.Close(); err != nil {
					t.Error(err)
				}
			}()
			if got := dashboardTerminalAffectedCells(t, emulator, example.output, 80, 2); got != example.cells {
				t.Errorf("affected cells: got %d, want %d", got, example.cells)
			}
		})
	}
}
