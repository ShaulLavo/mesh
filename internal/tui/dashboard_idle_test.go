package tui

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

func TestDashboardIdleTickOutput(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		t.Run(fmt.Sprintf("ascii=%v", ascii), func(t *testing.T) {
			model := dashboardPerformanceFixture()
			model.ascii = ascii
			// A quiet fleet with no graph samples still advances clocks and catalog ages.
			clear(model.history)
			before := model.render()
			next, command := model.Update(dashboardTickMsg(model.now.Add(time.Second)))
			if command == nil {
				t.Fatal("one-second clock stopped")
			}
			after := next.(dashboardModel).View().Content
			if before == after || !strings.Contains(ansi.Strip(after), model.now.Add(time.Second).Format("15:04:05")) {
				t.Fatal("idle clock failed to advance")
			}
			var output bytes.Buffer
			renderer := uv.NewTerminalRenderer(&output, []string{"TERM=xterm-256color"})
			renderer.SetFullscreen(true)
			renderer.SetColorProfile(model.profile)
			renderer.SetScrollOptim(true)
			renderer.SetTabStops(-1)
			screen := uv.NewScreenBuffer(model.width, model.height)
			draw := func(frame string) {
				t.Helper()
				screen.Clear()
				uv.NewStyledString(frame).Draw(screen, screen.Bounds())
				renderer.Render(screen.RenderBuffer)
				if err := renderer.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			emulator := vt.NewEmulator(model.width, model.height)
			defer func() {
				if err := emulator.Close(); err != nil {
					t.Error(err)
				}
			}()
			draw(before)
			if _, err := emulator.Write(output.Bytes()); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			draw(after)
			cells := dashboardTerminalAffectedCells(t, emulator, output.String(), model.width, model.height)
			t.Logf("idle tick: %d bytes, %d affected cells", output.Len(), cells)
			if output.Len() > 500 || cells > 80 {
				t.Errorf("idle tick rewrites quiet cells: %d bytes, %d affected cells (budget 500 bytes / 80 cells)", output.Len(), cells)
			}
			for y := range model.height {
				for x := range model.width {
					want, got := screen.CellAt(x, y), emulator.CellAt(x, y)
					if !dashboardTerminalCellEqual(want, got) {
						t.Fatalf("terminal differs from frame at %d,%d: want %#v, got %#v", x, y, want, got)
					}
				}
			}
			output.Reset()
			draw(after)
			if output.Len() != 0 {
				t.Fatalf("no-change frame emitted %d bytes", output.Len())
			}
		})
	}
}

func BenchmarkDashboardIdleTick(b *testing.B) {
	model := dashboardPerformanceFixture()
	clear(model.history)
	renderer := uv.NewTerminalRenderer(io.Discard, []string{"TERM=xterm-256color"})
	renderer.SetFullscreen(true)
	renderer.SetColorProfile(model.profile)
	renderer.SetScrollOptim(true)
	renderer.SetTabStops(-1)
	screen := uv.NewScreenBuffer(model.width, model.height)
	draw := func(frame string) {
		screen.Clear()
		uv.NewStyledString(frame).Draw(screen, screen.Bounds())
		renderer.Render(screen.RenderBuffer)
		if err := renderer.Flush(); err != nil {
			b.Fatal(err)
		}
	}
	draw(model.render())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		next, _ := model.Update(dashboardTickMsg(model.now.Add(time.Second)))
		model = next.(dashboardModel)
		draw(model.View().Content)
	}
}
