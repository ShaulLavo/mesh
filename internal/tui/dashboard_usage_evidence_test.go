package tui

import (
	"flag"
	"fmt"
	"html"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/usagefeed"
)

var usageEvidenceDirectory = flag.String("usage-evidence-dir", "", "write deterministic dashboard usage fixture SVGs and text grids")

// Evidence is the production renderer's terminal cells, not a separate layout implementation.
func TestDashboardUsageEvidence(t *testing.T) {
	if *usageEvidenceDirectory == "" {
		t.Skip("pass -args -usage-evidence-dir DIR to export fixture evidence")
	}
	if err := os.MkdirAll(*usageEvidenceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"normal", "no-data", "overflow", "mixed", "historic", "model-scoped"} {
		model := usageFixture(t, name)
		writeUsageEvidence(t, name, model)
		panel := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n")) + "\n"
		if err := os.WriteFile(filepath.Join(*usageEvidenceDirectory, name+"-panel.txt"), []byte(panel), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, palette := range dashboardPalettes {
		model := usageFixture(t, "normal")
		model.palette = dashboardTheme(palette.name)
		writeUsageEvidence(t, "theme-"+palette.name, model)
		model = usageFixture(t, "mixed")
		model.palette = dashboardTheme(palette.name)
		writeUsageEvidence(t, "theme-mixed-"+palette.name, model)
	}
	model := usageFixture(t, "normal")
	model.width, model.height = 80, 24
	writeUsageEvidence(t, "compact", model)
	for _, name := range []string{"no-data", "mixed", "historic", "model-scoped"} {
		fixture := usageFixture(t, name)
		fixture.width, fixture.height = 80, 24
		writeUsageEvidence(t, "compact-"+name, fixture)
	}
	model = usageFixture(t, "normal")
	model.ascii = true
	writeUsageEvidence(t, "ascii", model)
	model = usageFixture(t, "normal")
	model.hosts[0].Services.Rows[0].Failed = true
	model.hosts[0].Services.Rows[0].State = "unhealthy"
	model.hosts[0].Services.Rows[0].Problem = "health check refused"
	model.hosts[0].Services.Failed = 1
	model.hosts[0].Services.Idle = 0
	writeUsageEvidence(t, "failure", model)
	model.width, model.height = 80, 24
	writeUsageEvidence(t, "compact-failure", model)
	model.hosts[0].Services.Rows[0].Problem = "health check failed: connection refused while reaching the local service"
	writeUsageEvidence(t, "review-wrapped-compact", model)
	model.width, model.height = 160, 45
	writeUsageEvidence(t, "review-wrapped", model)
	model = usageFixture(t, "normal")
	seen := model.now.Add(-10 * time.Minute)
	model.usage.accounts[0].Windows[1].LastSeenAt = &seen
	writeUsageEvidence(t, "review-fresh-age", model)
	seen = model.now.Add(-18 * time.Minute)
	reset := model.now.Add(-time.Minute)
	model.usage.accounts[0].Windows[1].LastSeenAt = &seen
	model.usage.accounts[0].Windows[1].ResetsAt = &reset
	writeUsageEvidence(t, "review-reset", model)
	model.width, model.height = 80, 24
	writeUsageEvidence(t, "review-reset-compact", model)
	model = usageFixture(t, "normal")
	model.usage.accounts[0].Windows = model.usage.accounts[0].Windows[1:]
	model.usage.accounts[1].State = "cooldown"
	model.usage.accounts[1].LastSeenAt = nil
	model.usage.accounts[1].Windows = nil
	model.usage.accounts[1].Cooldown = model.usage.accounts[2].Cooldown
	model.usage.accounts[2].State = "disabled"
	writeUsageEvidence(t, "review-restrictions", model)
	model = usageFixture(t, "historic")
	recent := model.now.Add(-time.Minute)
	for index := range model.usage.accounts {
		model.usage.accounts[index].LastSeenAt = &recent
	}
	model.width, model.height = 80, 24
	writeUsageEvidence(t, "historic-independent-compact", model)
	model = usageFixture(t, "historic")
	model.usage.accounts = append(model.usage.accounts, model.usage.accounts[0])
	model.usage.total = len(model.usage.accounts)
	for index := range model.usage.accounts {
		account := &model.usage.accounts[index]
		window := account.Windows[0]
		account.LastSeenAt, account.Credits = &recent, nil
		switch index {
		case 0:
			warning := 97.0
			window.Status, window.UsedPercent = "warning", &warning
		case 1:
			window.Status, window.UsedPercent, window.LastSeenAt = usageUnknown, nil, nil
		case 2:
			seen := model.now.Add(-25 * time.Hour)
			window.LastSeenAt = &seen
		}
		account.Windows = []usagefeed.Window{window}
	}
	model.width, model.height = 80, 24
	writeUsageEvidence(t, "historic-status-compact", model)
	model = usageFixture(t, "historic")
	for index := range model.usage.accounts {
		account := &model.usage.accounts[index]
		window := account.Windows[0]
		account.LastSeenAt, account.Credits = &recent, nil
		seen := model.now.Add(-25 * time.Hour)
		window.LastSeenAt = &seen
		window.Status, window.UsedPercent = usageUnknown, nil
		if index == 1 {
			allowed := 20.0
			window.Status, window.UsedPercent = "allowed", &allowed
		}
		account.Windows = []usagefeed.Window{window}
	}
	model.width, model.height = 80, 24
	writeUsageEvidence(t, "historic-late-compact", model)
	model = usageFixture(t, "normal")
	model.ascii = true
	model.hosts[0].Services.ObservedAt = model.now.Add(-12 * time.Minute)
	writeUsageEvidence(t, "review-cached-service", model)
	model.width, model.height = 80, 24
	for index := range model.hosts {
		model.hosts[index].Connection = cli.StateUnreachable
	}
	writeUsageEvidence(t, "review-cached-compact", model)
	model = usageFixture(t, "overflow")
	model.width, model.height = 80, 24
	model.usageFailing = true
	writeUsageEvidence(t, "review-overflow-compact", model)
	for _, width := range []int{110, 140} {
		model = usageFixture(t, "normal")
		model.width = width
		writeUsageEvidence(t, fmt.Sprintf("width-%d", width), model)
	}
}

func writeUsageEvidence(t *testing.T, name string, model dashboardModel) {
	t.Helper()
	frame := model.render()
	assertFits(t, frame, model.width, model.height)
	terminal := vt.NewEmulator(model.width, model.height)
	defer func() {
		if err := terminal.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := terminal.WriteString(strings.ReplaceAll(frame, "\n", "\r\n")); err != nil {
		t.Fatal(err)
	}
	var svg strings.Builder
	fmt.Fprintf(&svg, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d"><rect width="100%%" height="100%%" fill="%s"/><g font-family="DejaVu Sans Mono,monospace" font-size="20">`, model.width*12, model.height*24, model.width*12, model.height*24, model.palette.background.hex)
	for y := range model.height {
		for x := range model.width {
			cell := terminal.CellAt(x, y)
			if cell == nil || cell.Content == "" || cell.Content == " " {
				continue
			}
			ink := model.palette.text.hex
			if cell.Style.Fg != nil {
				ink = usageEvidenceColor(cell.Style.Fg)
			}
			weight := "normal"
			if cell.Style.Attrs&uv.AttrBold != 0 {
				weight = "bold"
			}
			fmt.Fprintf(&svg, `<text x="%d" y="%d" fill="%s" font-weight="%s">%s</text>`, x*12, y*24+20, ink, weight, html.EscapeString(cell.Content))
		}
	}
	svg.WriteString("</g></svg>\n")
	if err := os.WriteFile(filepath.Join(*usageEvidenceDirectory, name+".svg"), []byte(svg.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*usageEvidenceDirectory, name+".txt"), []byte(ansi.Strip(frame)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
func usageEvidenceColor(value color.Color) string {
	r, g, b, _ := value.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}
