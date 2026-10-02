package tui

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/colorprofile"
)

func TestDashboardFrameByteParity(t *testing.T) {
	if *usageEvidenceDirectory != "" {
		if err := os.MkdirAll(*usageEvidenceDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var golden map[string]string
	if *usageEvidenceDirectory == "" {
		data, err := os.ReadFile("testdata/dashboard-frame-sha256.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &golden); err != nil {
			t.Fatal(err)
		}
	}
	for _, theme := range dashboardPalettes {
		for _, size := range [][2]int{{50, 12}, {60, 20}, {80, 24}, {110, 32}, {110, 45}, {140, 40}, {140, 45}, {160, 45}, {160, 48}} {
			for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI, colorprofile.ASCII} {
				for _, usage := range []bool{false, true} {
					name := fmt.Sprintf("matrix-%s-%dx%d-%d-usage-%t", theme.name, size[0], size[1], profile, usage)
					model := dashboardTickFixture(t, usage, 4)
					model.palette = theme
					model.width, model.height = size[0], size[1]
					model.profile, model.ascii = profile, profile != colorprofile.TrueColor
					frame := model.render()
					if *usageEvidenceDirectory != "" {
						writeUsageEvidence(t, name, model)
						if err := os.WriteFile(filepath.Join(*usageEvidenceDirectory, name+".ansi"), []byte(frame), 0o600); err != nil {
							t.Fatal(err)
						}
						continue
					}
					if got := fmt.Sprintf("%x", sha256.Sum256([]byte(frame))); got != golden[name] {
						t.Errorf("%s changed frame bytes: %s, want %s", name, got, golden[name])
					}
				}
			}
		}
	}
}
