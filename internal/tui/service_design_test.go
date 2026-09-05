package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"
)

func TestServedWebsiteDesignSpace(t *testing.T) {
	designs := []struct {
		name  string
		lines []string
	}{
		{name: "A: inline section", lines: inlineServicesSketch()},
		{name: "B: split row", lines: splitServicesSketch()},
		{name: "C: separate tab", lines: tabbedServicesSketch()},
	}

	frames := make([][]string, len(designs))
	for index, design := range designs {
		frames[index] = exactFrame(t, design.name, design.lines)
	}

	var output strings.Builder
	output.WriteString("Served-website picker comparison, each frame is exactly 80x24\n\n")
	output.WriteString("A keeps full URLs in the host view and takes rows from the screen preview. CHOSEN.\n")
	output.WriteString("B preserves preview height, but makes both session labels and URLs too narrow.\n")
	output.WriteString("C fits every field, but hides websites behind another mode.\n\n")
	for row := range designHeight {
		fmt.Fprintf(&output, "%s │ %s │ %s │\n", frames[0][row], frames[1][row], frames[2][row])
	}
	golden.RequireEqual(t, output.String())
}

func inlineServicesSketch() []string {
	return []string{
		" mesh / pc · pc.tail.example              online · 2 active · 2 served",
		" Choose a session, or start another one.",
		"",
		" > mesh · claude  detached · output 12s ago · 7K3D",
		"   site · npm     in use   · output 2s ago  · 91AZ",
		"",
		" served websites",
		"   healthy    https://pc.mesh.shaulavo.dev/blog",
		"   unhealthy  https://status.shaulavo.dev/api",
		"",
		" ┌─ mesh · claude  7K3D ─────────────────────────────────────────────────────┐",
		" │ current path  /home/shaul/src/mesh                                        │",
		" │ launched      claude --dangerously-skip-permissions                       │",
		" │ foreground    claude  ·  title mesh                                       │",
		" │ activity      detached  ·  output 12s ago  ·  started 4m ago              │",
		" │ screen        live                                                        │",
		" │ $ go test ./...                                                            │",
		" │ ok                                                                          │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" └─────────────────────────────────────────────────────────────────────────────┘",
		" ↑/↓  enter attach  space screen  n new  r resume  k kill  x rm  esc hosts",
	}
}

func splitServicesSketch() []string {
	return []string{
		" mesh / pc · pc.tail.example              online · 2 active · 2 served",
		" Choose a session, or start another one.",
		"",
		" SESSIONS                              SERVED WEBSITES",
		" > mesh · claude  detached · 7K3D      healthy https://pc.mesh.shaulavo...",
		"   site · npm     in use   · 91AZ      unhealthy https://status.shaulavo...",
		"",
		" ┌─ mesh · claude  7K3D ─────────────────────────────────────────────────────┐",
		" │ current path  /home/shaul/src/mesh                                        │",
		" │ launched      claude --dangerously-skip-permissions                       │",
		" │ foreground    claude  ·  title mesh                                       │",
		" │ activity      detached  ·  output 12s ago  ·  started 4m ago              │",
		" │ screen        live                                                        │",
		" │ $ go test ./...                                                            │",
		" │ ok                                                                          │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" └─────────────────────────────────────────────────────────────────────────────┘",
		" ↑/↓  enter attach  space screen  n new  r resume  k kill  x rm  esc hosts",
	}
}

func tabbedServicesSketch() []string {
	return []string{
		" mesh / pc · pc.tail.example              online · 2 active · 2 served",
		" [sessions]  served websites",
		"",
		" > mesh · claude  detached · output 12s ago · 7K3D",
		"   site · npm     in use   · output 2s ago  · 91AZ",
		"",
		" ┌─ mesh · claude  7K3D ─────────────────────────────────────────────────────┐",
		" │ current path  /home/shaul/src/mesh                                        │",
		" │ launched      claude --dangerously-skip-permissions                       │",
		" │ foreground    claude  ·  title mesh                                       │",
		" │ activity      detached  ·  output 12s ago  ·  started 4m ago              │",
		" │ screen        live                                                        │",
		" │ $ go test ./...                                                            │",
		" │ ok                                                                          │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" │                                                                             │",
		" └─────────────────────────────────────────────────────────────────────────────┘",
		" tab websites  ↑/↓ move  enter attach  space screen  esc hosts",
	}
}
