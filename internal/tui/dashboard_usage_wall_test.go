package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestDashboardUsageWallAccounts(t *testing.T) {
	for _, size := range [][2]int{{160, 45}, {344, 90}} {
		model := usageFixture(t, "wall-accounts")
		model.now = time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)
		model.width, model.height = size[0], size[1]
		frame := model.render()
		assertFits(t, frame, model.width, model.height)
		if *usageEvidenceDirectory != "" {
			writeUsageEvidence(t, "wall-accounts-"+dashboardDuration(time.Duration(size[0])*time.Second), model)
		}
		panel := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n"))
		for _, want := range []string{"3 accounts", "Claude", "account-1", "account-2", "account-3", "100% used", "43% used", "3% used", "stale 1h", "stale 2h", "stale 28m"} {
			if !strings.Contains(panel, want) {
				t.Errorf("%v missing %q:\n%s", size, want, panel)
			}
		}
		for _, unwanted := range []string{"Model", "gpt-", "Fable", "90%", "omitted"} {
			if strings.Contains(panel, unwanted) {
				t.Errorf("%v unwanted %q:\n%s", size, unwanted, panel)
			}
		}
		if strings.Count(panel, "43% used") != 1 {
			t.Errorf("repeated aggregate quota:\n%s", panel)
		}
	}
}
