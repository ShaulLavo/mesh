package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
)

const (
	designWidth  = 80
	designHeight = 24
)

func TestDesignSpace(t *testing.T) {
	designs := []struct {
		name  string
		lines []string
	}{
		{name: "A: two panes", lines: twoPaneSketch()},
		{name: "B: drill down", lines: drillDownSketch()},
		{name: "C: action palette", lines: actionPaletteSketch()},
	}

	frames := make([][]string, len(designs))
	for i, design := range designs {
		frames[i] = exactFrame(t, design.name, design.lines)
	}

	var output strings.Builder
	output.WriteString("T09 picker design comparison, each frame is exactly 80x24\n\n")
	output.WriteString("A keeps both levels visible, but truncates command and cwd at the target size.\n")
	output.WriteString("B adds one Enter/Esc step and gives session metadata the full terminal width. CHOSEN.\n")
	output.WriteString("C minimizes navigation, but mixes host actions with sessions and obscures hierarchy.\n\n")
	for row := range designHeight {
		fmt.Fprintf(&output, "%s │ %s │ %s │\n", frames[0][row], frames[1][row], frames[2][row])
	}
	golden.RequireEqual(t, output.String())
}

func exactFrame(t *testing.T, name string, lines []string) []string {
	t.Helper()
	if len(lines) != designHeight {
		t.Fatalf("%s has %d rows, want %d", name, len(lines), designHeight)
	}
	frame := make([]string, len(lines))
	for row, line := range lines {
		width := ansi.StringWidth(line)
		if width > designWidth {
			t.Fatalf("%s row %d has %d columns, want at most %d", name, row+1, width, designWidth)
		}
		frame[row] = line + strings.Repeat(" ", designWidth-width)
	}
	return frame
}

func twoPaneSketch() []string {
	return []string{
		" mesh  choose a host and session",
		" ",
		" HOSTS                     SESSIONS ON PC",
		" ┌──────────────────────┐  ┌──────────────────────────────────────────────────┐",
		" │ > pc       online   │  │ > 7K3D detached 4m claude --dangerously... │",
		" │   pi       offline  │  │   91AZ running  8m npm run dev          │",
		" │   vps      online   │  │   + new session                          │",
		" │                      │  │   ~ resume latest                        │",
		" │                      │  │                                          │",
		" │                      │  │ cwd and cache state do not fit here      │",
		" │                      │  │                                          │",
		" │                      │  │                                          │",
		" │                      │  │                                          │",
		" │                      │  │                                          │",
		" │                      │  │                                          │",
		" │                      │  │                                          │",
		" │                      │  │                                          │",
		" │                      │  │                                          │",
		" └──────────────────────┘  └──────────────────────────────────────────────────┘",
		" ",
		" ↑/↓ move host   tab move pane   enter select   esc quit",
		" ",
		" Trade-off: fastest overview; cramped session details at 80 columns.",
		" ",
	}
}

func drillDownSketch() []string {
	return []string{
		" mesh / pc                                      online  2 active",
		" Choose a session, or start another one.",
		" ",
		" > 7K3D  detached  4m  live",
		"   claude --dangerously-skip-permissions",
		"   ~/src/mesh",
		" ",
		"   91AZ  running   8m  live",
		"   npm run dev",
		"   ~/src/site",
		" ",
		"   C4P2  exited    21m  live",
		"   go test ./...",
		"   ~/src/mesh",
		" ",
		" ",
		" n new session       r resume latest",
		" ",
		" ",
		" ",
		" ↑/↓ move   enter attach   n new   r resume   esc hosts",
		" ",
		" Trade-off: one more step; full-width metadata and a clear hierarchy.",
		" ",
	}
}

func actionPaletteSketch() []string {
	return []string{
		" mesh  What do you want to do?",
		" ",
		" > attach  7K3D on pc        detached 4m",
		"   attach  91AZ on pc        running  8m",
		"   new     session on pc",
		"   resume  latest on pc",
		"   wake    pi                 offline",
		"   attach  Q8ME on pi         stale 2h",
		"   new     session on vps",
		"   attach  B2FX on vps        running 11m",
		" ",
		" ",
		" ",
		" ",
		" ",
		" ",
		" ",
		" ",
		" ",
		" ",
		" type to filter   ↑/↓ move   enter run   esc quit",
		" ",
		" Trade-off: fewest steps; host boundaries and stale context are weaker.",
		" ",
	}
}
