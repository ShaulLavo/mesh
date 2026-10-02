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

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
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
	for _, name := range []string{"normal", "no-data", "overflow"} {
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
	}
	model := usageFixture(t, "normal")
	model.width, model.height = 80, 24
	writeUsageEvidence(t, "compact", model)
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
