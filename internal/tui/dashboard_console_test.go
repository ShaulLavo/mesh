package tui

import (
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardCompactFailureIsRed(t *testing.T) {
	model := dashboardDesignFixture(4)
	for _, state := range []cli.StateConnection{cli.StateUnreachable, cli.StateRefused} {
		host := model.hosts[3]
		host.Connection = state
		row := model.fleetRow(host)
		if !strings.Contains(row, dashboardFailureStyle.Render(string(state))) {
			t.Fatalf("compact failure state has no failure color: %q", row)
		}
	}
}

func TestDashboardConsoleUsesExplicitStatusColors(t *testing.T) {
	model := dashboardDesignFixture(4)
	updated, _ := model.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI})
	model = updated.(dashboardModel)
	model.ascii = true
	cached := model.sessionRow(model.hosts[3], model.hosts[3].Sessions.Rows[0], 90)
	if !regexp.MustCompile(`\x1b\[(?:[0-9]+;)*(?:33|93)(?:;[0-9]+)*m`).MatchString(cached) || regexp.MustCompile(`\x1b\[(?:[0-9]+;)*(?:31|91)(?:;[0-9]+)*m`).MatchString(cached) {
		t.Fatalf("cached session must use explicit yellow and never red: %q", cached)
	}
	failure := model.fleetRow(model.hosts[3])
	if !regexp.MustCompile(`\x1b\[(?:[0-9]+;)*(?:31|91)(?:;[0-9]+)*m`).MatchString(failure) {
		t.Fatalf("unreachable host must use explicit red: %q", failure)
	}
	view := model.render()
	if strings.Contains(view, "38;2;") || strings.Contains(view, "38;5;") || strings.Contains(view, "48;2;") || strings.Contains(view, "48;5;") {
		t.Fatal("ANSI16 model retained colors requiring automatic quantization")
	}
	if !strings.Contains(dashboardDesignFixture(4).render(), "38;2;224;176;80") {
		t.Fatal("ANSI16 model changed another model's truecolor palette")
	}
	for _, code := range []string{"32", "36", "90"} {
		if !regexp.MustCompile(`\x1b\[(?:[0-9]+;)*` + code + `(?:;[0-9]+)*m`).MatchString(view) {
			t.Fatalf("missing explicit CPU/RAM/label SGR %q", code)
		}
	}
}

func TestDashboardLiveRowsBeforeCachedReservation(t *testing.T) {
	model := dashboardDesignFixture(4)
	rows := strings.Join(model.sessionSummary(90, 10), "\n")
	if !strings.Contains(rows, "live 6/7") || !strings.Contains(rows, "0/2 cached") {
		t.Fatalf("cached group reservation displaced available live rows: %s", rows)
	}
}
