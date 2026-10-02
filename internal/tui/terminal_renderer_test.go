package tui

import (
	"bytes"
	"fmt"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// Erasing plain spaces preserves their visible background, not their foreground.
func dashboardTerminalCellEqual(want, got *uv.Cell) bool {
	if want == nil || got == nil {
		return want == got
	}
	if want.Content != " " || got.Content != " " || want.Style.Underline != 0 || got.Style.Underline != 0 || want.Style.Attrs != 0 || got.Style.Attrs != 0 {
		return want.Equal(got)
	}
	left, right := *want, *got
	left.Style.Fg, right.Style.Fg = nil, nil
	return left.Equal(&right)
}

func TestTerminalRendererEqualSpans(t *testing.T) {
	for _, example := range []struct {
		name, prefix, gap string
		width, maxCells   int
	}{
		{"long", "", strings.Repeat("x", 60), 80, 2},
		{"short", "", "xx", 80, 4},
		{"colored", "", "\x1b[31m" + strings.Repeat("x", 60) + "\x1b[0m", 80, 2},
		{"late-gap", strings.Repeat("x", 110), "xxxxx", 160, 2},
		// Wide-cell drift deliberately erases the 80-cell row before repainting 42 cells.
		{"wide", "", strings.Repeat("界", 20), 80, 122}} {
		t.Run(example.name, func(t *testing.T) {
			var output bytes.Buffer
			renderer := uv.NewTerminalRenderer(&output, []string{"TERM=xterm-256color"})
			renderer.SetColorProfile(colorprofile.TrueColor)
			renderer.SetFullscreen(true)
			screen := uv.NewScreenBuffer(example.width, 2)
			emulator := vt.NewEmulator(example.width, 2)
			defer func() {
				if err := emulator.Close(); err != nil {
					t.Error(err)
				}
			}()
			for frame, endpoints := range []string{"ab", "cd", "ef", "ab"} {
				screen.Clear()
				uv.NewStyledString(example.prefix+endpoints[:1]+example.gap+endpoints[1:]+"\nretained second row").Draw(screen, screen.Bounds())
				renderer.Render(screen.RenderBuffer)
				if err := renderer.Flush(); err != nil {
					t.Fatal(err)
				}
				cells := dashboardTerminalAffectedCells(t, emulator, output.String(), example.width, 2)
				if frame > 0 && cells > example.maxCells {
					t.Errorf("equal span rewritten: %d affected cells: %q", cells, output.String())
				}
				for y := range 2 {
					for x := range example.width {
						if !dashboardTerminalCellEqual(screen.CellAt(x, y), emulator.CellAt(x, y)) {
							t.Fatalf("terminal differs at %d,%d after %s", x, y, endpoints)
						}
					}
				}
				output.Reset()
			}
		})
	}
}

func TestDashboardTerminalBlankCellEqual(t *testing.T) {
	for _, attrs := range []uint8{0, uv.AttrReverse, uv.AttrStrikethrough} {
		left := uv.Cell{Content: " ", Width: 1, Style: uv.Style{Fg: color.RGBA{R: 255, A: 255}, Attrs: attrs}}
		right := left
		right.Style.Fg = color.RGBA{B: 255, A: 255}
		if got := dashboardTerminalCellEqual(&left, &right); got != (attrs == 0) {
			t.Errorf("foreground visibility for attributes %d: equal=%v", attrs, got)
		}
	}
}

// Keep screen construction outside the measurement so only the diff is charged.
func terminalRendererGapUpdate(width int) func() {
	renderer := uv.NewTerminalRenderer(io.Discard, []string{"TERM=xterm-256color"})
	renderer.SetFullscreen(true)
	renderer.SetTabStops(-1)
	screen := uv.NewScreenBuffer(width, 2)
	uv.NewStyledString("a"+strings.Repeat("x", width-2)+"b\nretained second row").Draw(screen, screen.Bounds())
	renderer.Render(screen.RenderBuffer)
	_ = renderer.Flush()
	first := &uv.Cell{Content: "a", Width: 1}
	second := &uv.Cell{Content: "b", Width: 1}
	return func() {
		first, second = second, first
		screen.SetCell(0, 0, first)
		screen.SetCell(width-1, 0, second)
		renderer.Render(screen.RenderBuffer)
		_ = renderer.Flush()
	}
}

func TestTerminalRendererLongGapAllocations(t *testing.T) {
	for _, width := range []int{80, 160, 4096} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			allocations := testing.AllocsPerRun(10, terminalRendererGapUpdate(width))
			t.Logf("width %d: %.0f allocations/update", width, allocations)
			if allocations > 64 {
				t.Errorf("unchanged gap allocates with width: %.0f allocations (budget 64)", allocations)
			}
		})
	}
}

func BenchmarkTerminalRendererLongGap(b *testing.B) {
	for _, width := range []int{80, 160, 4096} {
		b.Run(fmt.Sprint(width), func(b *testing.B) {
			update := terminalRendererGapUpdate(width)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				update()
			}
		})
	}
}
