package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/privacy"
)

func TestDashboardConfigurationErrorIsRetainedAndActionable(t *testing.T) {
	problem := errors.New(`parse host config config/hosts.json: json: unknown field "unexpected"; remove the unrecognized field from this file and retry`)
	for _, terminal := range []string{"xterm-256color", "linux", "dumb"} {
		t.Run(terminal, func(t *testing.T) {
			now := time.Now()
			model := newDashboardForTerminal(cli.DashboardInput{Wall: true, ConfigError: problem}, now, terminal)
			view := ansi.Strip(model.View().Content)
			if !strings.Contains(view, "Configuration error") || !strings.Contains(view, "unexpected") || !strings.Contains(view, "hosts.json") || !strings.Contains(view, "retries every second") {
				t.Fatalf("missing actionable configuration error: %s", view)
			}
			if strings.Contains(view, "reachable 0") || strings.Contains(view, "Hosts 0 / 0") || strings.Contains(view, "sessions 0") {
				t.Fatal("invalid configuration rendered a healthy empty fleet")
			}
			updated, command := model.Update(dashboardTickMsg(now.Add(time.Second)))
			if command == nil {
				t.Fatal("error screen stopped its bounded tick")
			}
			if _, quitting := command().(tea.QuitMsg); quitting {
				t.Fatal("configuration failure quit the dashboard")
			}
			view = ansi.Strip(updated.(dashboardModel).View().Content)
			if !strings.Contains(view, "Configuration error") {
				t.Fatal("tick discarded the configuration error")
			}
			next, command := updated.Update(dashboardConfigMsg{err: errors.New("read host config config/hosts.json: permission denied; correct file permissions")})
			if command != nil || !strings.Contains(ansi.Strip(next.(dashboardModel).View().Content), "permission denied") {
				t.Fatal("new configuration failure was lost or quit the view")
			}
		})
	}
}

func TestDashboardConfigurationErrorPrivacyAndSafeText(t *testing.T) {
	problem := errors.New("private/path/hosts.json: invalid value \x1b[31m")
	model := newDashboard(cli.DashboardInput{ConfigError: problem, Privacy: privacy.New()}, time.Now())
	view := ansi.Strip(model.View().Content)
	if strings.Contains(view, "private/path") || !strings.Contains(view, "Configuration error") {
		t.Fatal("configuration error bypassed privacy")
	}
	unmasked := newDashboard(cli.DashboardInput{ConfigError: problem}, time.Now())
	if strings.Contains(unmasked.configError, "\x1b") {
		t.Fatal("configuration error retained terminal escape sequences")
	}
}
