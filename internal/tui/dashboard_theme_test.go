package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardEveryThemeRendersWithApprovedRoles(t *testing.T) {
	cases := []struct{ name, background, colors string }{
		{"current", "#0d1117", "#cfdbd4 #b39df3 #7bd88f #183c27 #153d49 #17261f #56c8e8 #7a8f86 #24453a #7bd88f #e0b050 #ef6b5b #5fd7d7"},
		{"rose-pine", "#191724", "#e0def4 #ebbcba #9ccfd8 #363f4c #3f374f #26233a #c4a7e7 #908caa #403d52 #9ccfd8 #f6c177 #eb6f92 #c4a7e7"},
		{"rose-pine-moon", "#232136", "#e0def4 #ea9a97 #9ccfd8 #3e475a #463e5d #2a283e #c4a7e7 #908caa #44415a #9ccfd8 #f6c177 #eb6f92 #c4a7e7"},
		{"oled", "#000000", "#f2f7f4 #c88cff #4dff91 #0a2e18 #062a36 #1b2c25 #33d1ff #9aaba3 #2c5244 #4dff91 #ffc23d #ff5a52 #3cf2f2"},
		{"kanagawa", "#1f1f28", "#dcd7ba #957fb8 #98bb6c #3a4137 #34404c #2a2a37 #7fb4ca #938aa9 #363646 #98bb6c #e6c384 #ff5d62 #7e9cd8"},
		{"gruvbox-material", "#1d2021", "#d4be98 #d3869b #a9b665 #3c4130 #323f3e #282828 #7daea3 #a89984 #3c3836 #a9b665 #d8a657 #ea6962 #89b482"},
	}
	if len(cases) != len(cli.DashboardThemeNames()) || len(cases) != len(dashboardPalettes) {
		t.Fatal("theme inventory differs")
	}
	for _, example := range cases {
		t.Run(example.name, func(t *testing.T) {
			if err := cli.ValidateDashboardTheme(example.name); err != nil {
				t.Fatal(err)
			}
			model := dashboardPerformanceFixture()
			model.palette = newDashboard(cli.DashboardInput{Theme: example.name}, model.now).palette
			if model.palette.name != example.name {
				t.Fatalf("selected palette %q", model.palette.name)
			}
			before := dashboardPerformanceFixture().render()
			for index, value := range strings.Fields(example.colors) {
				role := dashboardStyle(index)
				if got := model.paint(role).GetForeground(); got != lipgloss.Color(value) {
					t.Errorf("role %d = %v, want %s", role, got, value)
				}
			}
			if model.View().BackgroundColor != lipgloss.Color(example.background) {
				t.Fatal("theme background not applied uniformly")
			}
			if model.palette.battery != model.palette.good {
				t.Fatal("battery role changed")
			}
			for _, size := range [][2]int{{160, 45}, {80, 24}} {
				model.width, model.height = size[0], size[1]
				frame := model.render()
				assertFits(t, frame, size[0], size[1])
				plain := ansi.Strip(frame)
				for _, word := range []string{"MESH fleet", "CPU", "RAM", "Sessions", "Services", "unhealthy"} {
					if !strings.Contains(plain, word) {
						t.Errorf("%dx%d lost %q", size[0], size[1], word)
					}
				}
				if strings.Contains(frame, "48;2;") || strings.Contains(frame, "48;5;") {
					t.Fatal("nested style overrides terminal background")
				}
			}
			if dashboardPerformanceFixture().render() != before {
				t.Fatal("theme changed another model")
			}
		})
	}
}

func TestDashboardEveryThemeANSI16Fallback(t *testing.T) {
	slots := []uint8{7, 5, 2, 2, 6, 8, 6, 8, 8, 2, 3, 1, 14}
	for _, name := range cli.DashboardThemeNames() {
		t.Run(name, func(t *testing.T) {
			model := dashboardPerformanceFixture()
			model.palette = dashboardTheme(name)
			model.profile = colorprofile.ANSI
			model.ascii = true
			for index, slot := range slots {
				role := dashboardStyle(index)
				paint := model.paint(role)
				if paint.GetForeground() != ansi.BasicColor(slot) {
					t.Errorf("role %d did not use ANSI slot %d", role, slot)
				}
				faint := role == dashboardCPUFillStyle || role == dashboardRAMFillStyle
				if paint.GetFaint() != faint {
					t.Errorf("role %d faint=%v, want %v", role, paint.GetFaint(), faint)
				}
			}
			view := model.View()
			if view.BackgroundColor != lipgloss.Black || view.ForegroundColor != lipgloss.White {
				t.Fatal("console defaults differ from fallback")
			}
			frame := model.render()
			assertFits(t, frame, 160, 45)
			if strings.Contains(frame, "38;2;") || strings.Contains(frame, "38;5;") || strings.Contains(frame, "48;") {
				t.Fatal("console frame retained extended colors")
			}
			if strings.ContainsAny(ansi.Strip(frame), "┌┐└┘▁▂▃▄▅▆▇█▪●⠉⠒⠤⣀") {
				t.Fatal("console frame retained Unicode marks")
			}
			model.profile = colorprofile.ASCII
			if model.View().BackgroundColor != nil || model.View().ForegroundColor != nil {
				t.Fatal("dumb terminal received color defaults")
			}
		})
	}
}

func TestDashboardDefaultOLED160x45Evidence(t *testing.T) {
	model := dashboardPerformanceFixture()
	if model.palette.name != cli.DefaultDashboardTheme {
		t.Fatal("OLED is not the default")
	}
	frame := model.render()
	assertFits(t, frame, 160, 45)
	for _, code := range []string{"38;2;77;255;145", "38;2;10;46;24", "38;2;51;209;255", "38;2;6;42;54"} {
		if !strings.Contains(frame, code) {
			t.Errorf("OLED edge or dim fill missing %s", code)
		}
	}
	fmt.Printf("\nBEGIN_OLED_160_45\n%s\nEND_OLED_160_45\n", frame)
}

func TestDashboardThemeTerminalDefaultsRestoreOnExit(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output := dashboardThemeOutput{rendered: make(chan struct{})}
	input := cli.DashboardInput{Wall: true, Watch: func(ctx context.Context, _ func(cli.DashboardHostView)) error {
		select {
		case <-output.rendered:
			cancel()
		case <-ctx.Done():
		}
		return nil
	}}
	if err := runDashboard(ctx, input, &output, tea.WithWindowSize(160, 45), tea.WithColorProfile(colorprofile.TrueColor)); err != nil {
		t.Fatal(err)
	}
	output.mu.Lock()
	frame := output.data.String()
	output.mu.Unlock()
	if !strings.Contains(ansi.Strip(frame), "MESH fleet") {
		t.Fatal("terminal never rendered the dashboard")
	}
	for _, sequence := range []string{ansi.SetBackgroundColor("#000000"), ansi.SetForegroundColor("#f2f7f4"), ansi.ResetBackgroundColor, ansi.ResetForegroundColor} {
		if !strings.Contains(frame, sequence) {
			t.Errorf("missing default-color lifecycle sequence %q", sequence)
		}
	}
}

type dashboardThemeOutput struct {
	dashboardTestOutput
	rendered chan struct{}
	once     sync.Once
}

func (w *dashboardThemeOutput) Write(value []byte) (int, error) {
	n, err := w.dashboardTestOutput.Write(value)
	w.mu.Lock()
	ready := strings.Contains(ansi.Strip(w.data.String()), "MESH fleet")
	w.mu.Unlock()
	if ready {
		w.once.Do(func() { close(w.rendered) })
	}
	return n, err
}

func TestDashboardDumbInitialViewOmitsThemeColorsBeforeProfileMessage(t *testing.T) {
	model := newDashboardForTerminal(cli.DashboardInput{Theme: "rose-pine", Wall: true}, time.Unix(1700000000, 0), "dumb")
	first := model.View()
	if first.BackgroundColor != nil || first.ForegroundColor != nil {
		t.Error("first dumb-terminal View selected theme defaults before a profile message")
	}
	if first.Content != ansi.Strip(first.Content) {
		t.Error("first dumb-terminal View emitted ANSI styling before a profile message")
	}
	if !strings.Contains(ansi.Strip(first.Content), "MESH fleet") {
		t.Fatal("first view did not render the dashboard")
	}
	updated, _ := model.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	later := updated.(dashboardModel).View()
	if later.BackgroundColor != lipgloss.Color("#191724") || later.ForegroundColor != lipgloss.Color("#e0def4") {
		t.Fatal("supported profile lost selected theme defaults")
	}
	if !strings.Contains(later.Content, "38;2;196;167;231") {
		t.Fatal("supported profile lost selected theme title color")
	}
}
