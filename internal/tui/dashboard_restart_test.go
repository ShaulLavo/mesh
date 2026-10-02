package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardRestartNoticeStaysOnOneLine(t *testing.T) {
	for _, usage := range []bool{false, true} {
		input := cli.DashboardInput{Wall: true, Notice: "Dashboard restart failed: permission denied"}
		if usage {
			input.UsageWatch = func(_ context.Context, _ func(usagefeed.Result)) error { return nil }
		}
		model := newDashboard(input, time.Now())
		model.width, model.height = 140, 40
		lines := strings.Split(ansi.Strip(model.render()), "\n")
		if !strings.Contains(lines[len(lines)-1], input.Notice) {
			t.Fatalf("restart notice missing from final row: %q", lines[len(lines)-1])
		}
		if strings.Count(strings.Join(lines, "\n"), input.Notice) != 1 {
			t.Fatal("restart notice repeated")
		}
	}
}
