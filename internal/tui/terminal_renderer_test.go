package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Erasing plain spaces preserves their visible background, not their foreground.
func dashboardTerminalCellEqual(want, got *uv.Cell) bool {
	if want == nil || got == nil || want.Content != " " || got.Content != " " || want.Style.Underline != 0 || got.Style.Underline != 0 {
		return want.Equal(got)
	}
	left, right := *want, *got
	left.Style.Fg, right.Style.Fg = nil, nil
	return left.Equal(&right)
}

func TestTerminalRendererEqualSpans(t *testing.T) {
	for _, example := range []struct {
		name, gap string
		maxCells  int
	}{
		{"long", strings.Repeat("x", 60), 2},
		{"short", "xx", 4},
		{"colored", "\x1b[31m" + strings.Repeat("x", 60) + "\x1b[0m", 2},
		{"wide", strings.Repeat("界", 20), 42}} {
		t.Run(example.name, func(t *testing.T) {
			var output bytes.Buffer
			renderer := uv.NewTerminalRenderer(&output, []string{"TERM=xterm-256color"})
			renderer.SetColorProfile(colorprofile.TrueColor)
			renderer.SetFullscreen(true)
			screen := uv.NewScreenBuffer(80, 2)
			emulator := vt.NewEmulator(80, 2)
			defer func() {
				if err := emulator.Close(); err != nil {
					t.Error(err)
				}
			}()
			for _, endpoints := range []string{"ab", "cd", "ef", "ab"} {
				screen.Clear()
				uv.NewStyledString(endpoints[:1]+example.gap+endpoints[1:]+"\nretained second row").Draw(screen, screen.Bounds())
				renderer.Render(screen.RenderBuffer)
				if err := renderer.Flush(); err != nil {
					t.Fatal(err)
				}
				if endpoints != "ab" && ansi.StringWidth(ansi.Strip(output.String())) > example.maxCells {
					t.Errorf("equal span rewritten: %q", output.String())
				}
				if _, err := emulator.Write(output.Bytes()); err != nil {
					t.Fatal(err)
				}
				for y := range 2 {
					for x := range 80 {
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
