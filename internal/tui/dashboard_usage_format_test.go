package tui

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardUsageWholeCredits(t *testing.T) {
	for _, test := range []struct {
		name    string
		credits *usagefeed.Credits
		want    string
	}{
		{"absent", nil, ""},
		{"zero", &usagefeed.Credits{}, ""},
		{"rounds to zero", &usagefeed.Credits{Balance: 0.4}, ""},
		{"fraction", &usagefeed.Credits{Balance: 62113.897503}, "credits 62,114"},
		{"round down", &usagefeed.Credits{Balance: 12.4}, "credits 12"},
		{"half credit", &usagefeed.Credits{Balance: 12.5}, "credits 13"},
		{"group carry", &usagefeed.Credits{Balance: 999.5}, "credits 1,000"},
		{"million", &usagefeed.Credits{Balance: 1234567.8}, "credits 1,234,568"},
		{"unlimited", &usagefeed.Credits{Unlimited: true}, "credits unlimited"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := usageCredits(test.credits); got != test.want {
				t.Errorf("credits = %q, want %q", got, test.want)
			}
			for _, compact := range []bool{false, true} {
				model := usageFixture(t, "normal")
				account := model.usage.accounts[0]
				account.Credits = test.credits
				rows := model.usageAccountWindows(account, 50, compact)
				wantRows := 4
				if compact {
					wantRows = 2
				}
				if test.want != "" {
					wantRows++
					if rows[0] != test.want {
						t.Errorf("credit row = %q, want %q", rows[0], test.want)
					}
				}
				if len(rows) != wantRows || usageAccountRows(account, compact) != wantRows+1 {
					t.Errorf("compact=%v rows=%d budget=%d, want %d rows and %d budget", compact, len(rows), usageAccountRows(account, compact), wantRows, wantRows+1)
				}
				account.Windows = nil
				empty := strings.Join(model.usageAccountWindows(account, 50, compact), "\n")
				if (test.want != "") != strings.Contains(empty, "credits ") {
					t.Errorf("empty summary credit visibility = %q", empty)
				}
			}
		})
	}
}

func TestDashboardUsageWindowSpacing(t *testing.T) {
	model := usageFixture(t, "normal")
	model.profile = colorprofile.TrueColor
	for _, label := range []string{"Session", "Weekly", "会話", "\x1b[1mSession\x1b[0m"} {
		for _, percent := range []float64{5, 20, 100} {
			for _, width := range []int{36, 50, 96} {
				t.Run(fmt.Sprintf("%s/%g/%d", ansi.Strip(label), percent, width), func(t *testing.T) {
					window := model.usage.accounts[0].Windows[0]
					window.Label, window.UsedPercent = label, &percent
					window.ResetsAt = nil
					lines := model.usageWindowLines(window, width, false)
					prefix := ansi.Strip(dashboardFit(label, 7)) + fmt.Sprintf(" %.0f%% used", percent)
					if !strings.HasPrefix(ansi.Strip(lines[0]), prefix) {
						t.Errorf("wide label/value separator missing: %q; want prefix %q", ansi.Strip(lines[0]), prefix)
					}
					compact := model.usageCompactWindow(window, width, false)
					if !strings.HasPrefix(ansi.Strip(compact), ansi.Strip(label)+fmt.Sprintf(" %.0f%%/", percent)) {
						t.Errorf("compact label/value separator missing: %q", ansi.Strip(compact))
					}
					for _, line := range append(lines, compact) {
						if ansi.StringWidth(line) > width {
							t.Errorf("line exceeds %d cells: %q", width, line)
						}
					}
				})
			}
		}
	}
}

func TestDashboardUsageMetadataSpacing(t *testing.T) {
	model := usageFixture(t, "normal")
	model.profile = colorprofile.TrueColor
	seen := model.now.Add(-3 * time.Minute)
	for _, label := range []string{"Account", "利用者", "\x1b[1mAccount\x1b[0m"} {
		account := dashboardUsageAccount{Account: usagefeed.Account{Provider: "codex", Label: label, Plan: "pro", LastSeenAt: &seen}, first: true, extraWindows: 1}
		identity := "Codex · " + ansi.Strip(label) + " · Pro · +1 window"
		age := "seen 3m"
		for _, compact := range []bool{false, true} {
			for _, width := range []int{36, 50, ansi.StringWidth(identity + age), ansi.StringWidth(identity+age) + 1} {
				line := model.usageIdentity(account, width, compact)
				plain := ansi.Strip(line)
				if ansi.StringWidth(line) != width || !strings.HasSuffix(plain, " "+age) {
					t.Errorf("compact=%v width=%d metadata needs a reserved gap: %q", compact, width, plain)
				}
				if width >= ansi.StringWidth(identity+" "+age) && !strings.Contains(plain, "+1 window") {
					t.Errorf("metadata truncates despite enough space: %q", plain)
				}
			}
		}
	}
}

type usageFormatTransport struct{ body []byte }

func (transport usageFormatTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(transport.body)), Header: make(http.Header)}, nil
}

func TestDashboardUsageFregatV1Format(t *testing.T) {
	if *usageEvidenceDirectory != "" {
		if err := os.MkdirAll(*usageEvidenceDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"fresh", "aged"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/usage/fregat-v1-" + name + ".json") //nolint:gosec // name selects a checked-in fixture from the fixed list
			if err != nil {
				t.Fatal(err)
			}
			feed, err := usagefeed.New(usagefeed.Config{URL: "https://feed.example.test/v1.json", Client: &http.Client{Transport: usageFormatTransport{body}}})
			if err != nil {
				t.Fatal(err)
			}
			result := feed.Refresh(t.Context())
			if result.Err != nil || result.Failing || result.Snapshot == nil {
				t.Fatalf("v1 producer fixture rejected: %v", result.Err)
			}
			model := dashboardUsageFleetFixture()
			model.now = result.Snapshot.GeneratedAt
			model.usageEnabled = true
			model.usage = projectDashboardUsage(result.Snapshot)
			if model.usage.total != len(result.Snapshot.Accounts) || *model.usage.accounts[0].Windows[0].UsedPercent != 5 {
				t.Fatal("producer fixture projection changed")
			}
			for _, compact := range []bool{false, true} {
				panel := ansi.Strip(strings.Join(model.usagePanel(100, 60, compact), "\n"))
				if strings.Contains(panel, "Session5%") || strings.Contains(panel, "Session20%") {
					t.Errorf("producer label/value joined: compact=%v\n%s", compact, panel)
				}
				if !strings.Contains(panel, "Session 5%") || !strings.Contains(panel, "Weekly") {
					t.Errorf("producer allowance lost: compact=%v\n%s", compact, panel)
				}
				if compact {
					model.width, model.height = 80, 24
				}
				assertFits(t, model.render(), model.width, model.height)
				if *usageEvidenceDirectory != "" {
					writeUsageEvidence(t, fmt.Sprintf("fregat-v1-%s-compact-%v", name, compact), model)
				}
			}
			model.usage.accounts[0].Credits = &usagefeed.Credits{Balance: 62113.897503}
			model.usage.accounts[1].Credits = &usagefeed.Credits{}
			for _, compact := range []bool{false, true} {
				model.width, model.height = 160, 45
				if compact {
					model.width, model.height = 80, 24
				}
				panel := ansi.Strip(strings.Join(model.usagePanel(100, 60, compact), "\n"))
				if !strings.Contains(panel, "credits 62,114") || strings.Contains(panel, "credits 0") || strings.Contains(panel, ".897503") {
					t.Errorf("producer credit presentation: compact=%v\n%s", compact, panel)
				}
				if *usageEvidenceDirectory != "" {
					writeUsageEvidence(t, fmt.Sprintf("fregat-v1-%s-credits-compact-%v", name, compact), model)
				}
			}
		})
	}
}
