package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/usagefeed"
)

func TestDashboardUsageEmptyAccounts(t *testing.T) {
	for _, state := range []string{"disabled", "no-data"} {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/compact=%v", state, compact), func(t *testing.T) {
				model := usageFixture(t, "no-data")
				account := model.usage.accounts[1].Account
				account.State = state
				for _, windows := range [][]usagefeed.Window{nil, {{Label: "5h", Status: usageUnknown}, {Label: "Weekly", Status: usageUnknown}}} {
					account.Windows = windows
					model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{account}})
					rows := model.usagePanel(54, 17, compact)
					got := ansi.Strip(strings.Join(rows, "\n"))
					want := "No reading yet · waits for traffic"
					if state == "disabled" {
						want = "Out of rotation · no reading"
					}
					if len(rows) != 4 || !strings.Contains(got, want) {
						t.Errorf("one identity and one empty summary required: %d rows\n%s", len(rows), got)
					}
					for _, placeholder := range []string{"5h", "Weekly", "No data yet", "Waiting for normal traffic"} {
						if strings.Contains(got, placeholder) {
							t.Errorf("per-window placeholder %q survives\n%s", placeholder, got)
						}
					}
				}
			})
		}
	}
}

func TestDashboardUsageEmptyRowsGoToDataAccounts(t *testing.T) {
	for _, compact := range []bool{false, true} {
		model := usageFixture(t, "normal")
		empty := usageFixture(t, "no-data").usage.accounts[1].Account
		empty.State = "disabled"
		accounts := []usagefeed.Account{empty}
		for index, observed := range model.usage.accounts {
			account := observed.Account
			account.Provider = "codex"
			account.Label = fmt.Sprintf("reading-%d", index)
			accounts = append(accounts, account)
		}
		model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: accounts})
		budget := 19
		if compact {
			budget = 13
		}
		rows := model.usagePanel(54, budget, compact)
		got := ansi.Strip(strings.Join(rows, "\n"))
		if len(rows) != budget || strings.Contains(got, "omitted") {
			t.Errorf("freed rows must admit all data accounts (compact=%v):\n%s", compact, got)
		}
		for _, want := range []string{"reading-0", "reading-1", "reading-2", "exhausted", "Out of rotation · no reading"} {
			if !strings.Contains(got, want) {
				t.Errorf("compact=%v missing %q\n%s", compact, want, got)
			}
		}
	}
}

func TestDashboardUsageHistoricDisabledWindow(t *testing.T) {
	model := usageFixture(t, "normal")
	account := model.usage.accounts[1].Account
	account.State = "disabled"
	old := model.now.Add(-24 * time.Hour)
	reset := model.now.Add(94 * time.Hour)
	used := 100.0
	account.LastSeenAt = &old
	account.Windows = []usagefeed.Window{{ID: "weekly", Label: "Weekly", UsedPercent: &used, LastSeenAt: &old, ResetsAt: &reset, Status: usageExhausted, Source: "proxy-state"}}
	account = validatedUsageAccount(t, model, account)
	for _, compact := range []bool{false, true} {
		model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{account}})
		got := ansi.Strip(strings.Join(model.usagePanel(54, 17, compact), "\n"))
		for _, want := range []string{"Weekly", "100%", "exhausted", "stale 1d", "resets"} {
			if !strings.Contains(got, want) {
				t.Errorf("compact=%v missing %q\n%s", compact, want, got)
			}
		}
		if strings.Contains(got, "No data") || strings.Contains(got, "no reading") || strings.Contains(got, "│▪") {
			t.Errorf("historic reading lost or pace refilled: %s", got)
		}
	}
}

func TestDashboardUsageKnownResetWithoutPercentage(t *testing.T) {
	model := usageFixture(t, "no-data")
	account := model.usage.accounts[0].Account
	reset := model.now.Add(94 * time.Hour)
	account.Windows = []usagefeed.Window{{ID: "weekly", Label: "Weekly", Status: usageUnknown, ResetsAt: &reset, Source: "proxy-state"}}
	account = validatedUsageAccount(t, model, account)
	for _, compact := range []bool{false, true} {
		got := ansi.Strip(strings.Join(model.usageAccountWindows(projectUsageAccount(account, true), 50, compact), "\n"))
		if !strings.Contains(got, "resets") || !strings.Contains(got, "3d") || strings.Contains(got, "0%") {
			t.Errorf("compact=%v reset fact missing or invented quota: %s", compact, got)
		}
	}
}

func TestDashboardUsageHistoricSourcesStayStale(t *testing.T) {
	for _, source := range []string{"reset-order", "future-provider-cache"} {
		model := usageFixture(t, "normal")
		account := model.usage.accounts[0].Account
		account.Windows[0].Source = source
		account = validatedUsageAccount(t, model, account)
		for _, compact := range []bool{false, true} {
			model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{account}})
			rows := model.usagePanel(54, 17, compact)
			got := ansi.Strip(strings.Join(rows, "\n"))
			if !strings.Contains(got, "stale 2m") || !strings.Contains(got, "20%") || model.usagePace(account.Windows[0]) != -1 {
				t.Errorf("source=%s compact=%v presented as fresh or lost last known usage: %s", source, compact, got)
			}
		}
	}
}

func TestDashboardUsageCredits(t *testing.T) {
	for _, credits := range []struct {
		value *usagefeed.Credits
		want  string
	}{
		{nil, ""},
		{&usagefeed.Credits{Balance: 62500}, "credits 62,500"},
		{&usagefeed.Credits{Unlimited: true}, "credits unlimited"},
		{&usagefeed.Credits{}, ""},
		{&usagefeed.Credits{Balance: 1234.5}, "credits 1,235"},
	} {
		for _, fixture := range []string{"normal", "no-data"} {
			for _, compact := range []bool{false, true} {
				model := usageFixture(t, fixture)
				account := model.usage.accounts[0].Account
				account.Label = "sample"
				account.Credits = credits.value
				model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{account}})
				width := 54
				if compact {
					width = 40
				}
				got := ansi.Strip(strings.Join(model.usagePanel(width, 17, compact), "\n"))
				if credits.want != "" && !strings.Contains(got, credits.want) {
					t.Errorf("%s compact=%v missing %q:\n%s", fixture, compact, credits.want, got)
				}
				if credits.want == "" && strings.Contains(got, "credits") {
					t.Fatal("absent credits manufactured", got)
				}
			}
		}
	}
}

func TestDashboardUsageEmptyLayoutAndViewBudgets(t *testing.T) {
	for _, name := range []string{"no-data", "mixed", "historic"} {
		for _, theme := range dashboardPalettes {
			for _, size := range [][2]int{{160, 45}, {80, 24}, {140, 40}} {
				model := usageFixture(t, name)
				model.palette = theme
				model.width, model.height = size[0], size[1]
				summaries := model.summaries(model.height)
				if size[0] >= 140 && model.summariesHeight(model.height) != len(summaries) {
					t.Fatalf("%s %s size=%v measured=%d actual=%d", name, theme.name, size, model.summariesHeight(model.height), len(summaries))
				}
				frame := model.render()
				assertFits(t, frame, size[0], size[1])
				next, _ := model.Update(dashboardTickMsg(model.now.Add(time.Second)))
				model = next.(dashboardModel)
				if allocations := testing.AllocsPerRun(100, func() { _ = model.View() }); allocations != 0 {
					t.Fatalf("%s %s retained View allocated %v", name, theme.name, allocations)
				}
			}
		}
	}
}

func TestDashboardUsageHistoricCompactIdentityAndWindow(t *testing.T) {
	model := usageFixture(t, "historic")
	model.width, model.height = 80, 24
	frame := ansi.Strip(model.render())
	if strings.Count(frame, "Weekly 100%/0% resets 3d22h used up") != 2 || strings.Count(frame, "stale 1d") != 2 {
		t.Fatal("historic label, used-up status, countdown or actual age clipped", frame)
	}
}

func TestDashboardUsageReviewReadableWindowProjection(t *testing.T) {
	model := usageFixture(t, "normal")
	account := model.usage.accounts[0].Account
	weekly := account.Windows[1]
	account.Windows = []usagefeed.Window{{Label: "Unobserved", Status: usageUnknown}, {Label: "Unknown", Status: usageUnknown}, weekly}
	projected := projectUsageAccount(account, true)
	model.usage = dashboardUsage{accounts: []dashboardUsageAccount{projected}, total: 1}
	got := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n"))
	if !strings.Contains(got, "Weekly  66% used") || strings.Contains(got, "No reading") || projected.extraWindows != 0 {
		t.Fatal("unobserved windows hid a real reading", got, projected.extraWindows)
	}
}

func TestDashboardUsageReviewIndependentHistoricCompact(t *testing.T) {
	model := usageFixture(t, "historic")
	recent := model.now.Add(-time.Minute)
	for index := range model.usage.accounts {
		model.usage.accounts[index].LastSeenAt = &recent
	}
	model.width, model.height = 80, 24
	got := ansi.Strip(model.render())
	if strings.Count(got, "Weekly used up resets 3d22h stale 1d") != 2 {
		t.Fatal("independent historic identity, reset, exhaustion or age clipped", got)
	}
}

func TestDashboardUsageReviewCompactStatusAndAge(t *testing.T) {
	allowed, warning, exhausted := 20.0, 97.0, 100.0
	for _, test := range []struct {
		name, status, word, age string
		used                    *float64
		seen                    time.Duration
	}{
		{name: "warning", status: "warning", word: "high", age: "stale 1d", used: &warning, seen: 24 * time.Hour},
		{name: "unknown-reset", status: usageUnknown, word: "unknown", age: "stale ?"},
		{name: "exhausted-25h", status: usageExhausted, word: "spent", age: "stale 1d1h", used: &exhausted, seen: 25 * time.Hour},
		{name: "unknown-25h", status: usageUnknown, word: "?", age: "stale 1d1h", seen: 25 * time.Hour},
		{name: "allowed-25h", status: "allowed", word: "OK", age: "stale 1d1h", used: &allowed, seen: 25 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := usageFixture(t, "historic")
			window := model.usage.accounts[0].Windows[0]
			window.Status, window.UsedPercent = test.status, test.used
			window.LastSeenAt = nil
			if test.seen > 0 {
				seen := model.now.Add(-test.seen)
				window.LastSeenAt = &seen
			}
			line := ansi.Strip(model.usageCompactWindow(window, 36, true))
			for _, fact := range []string{"Weekly", "resets 3d22h", test.word, test.age} {
				if !strings.Contains(line, fact) {
					t.Fatalf("missing %q in compact history %q", fact, line)
				}
			}
			if ansi.StringWidth(line) > 36 {
				t.Fatal("compact history overflow", line)
			}
		})
	}
}

func TestDashboardUsageReviewUnknownHistoricAgeExhaustion(t *testing.T) {
	model := usageFixture(t, "normal")
	account := model.usage.accounts[0].Account
	reset := model.now.Add(94 * time.Hour)
	account.Windows[1] = usagefeed.Window{ID: "weekly", Label: "Weekly", Status: usageExhausted, ResetsAt: &reset, Source: "reset-order"}
	account = validatedUsageAccount(t, model, account)
	model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{account}})
	got := ansi.Strip(strings.Join(model.usagePanel(54, 17, false), "\n"))
	if !strings.Contains(got, "exhausted") || !strings.Contains(got, "stale · age unknown") || !strings.Contains(got, "resets 3d 22h") {
		t.Fatal("unknown-age historic quota lost its used-up status", got)
	}
}

func TestDashboardUsageReviewEmptyCreditsAwaitTraffic(t *testing.T) {
	for _, credits := range []*usagefeed.Credits{{Balance: 62500}, {Unlimited: true}} {
		for _, width := range []int{54, 40} {
			model := usageFixture(t, "no-data")
			account := model.usage.accounts[0].Account
			account.Credits = credits
			model.usage = projectDashboardUsage(&usagefeed.Snapshot{Accounts: []usagefeed.Account{account}})
			rows := model.usagePanel(width, 17, width == 40)
			got := ansi.Strip(strings.Join(rows, "\n"))
			if len(rows) != 4 || !strings.Contains(got, "traffic") || !strings.Contains(got, usageCredits(credits)) {
				t.Fatal("empty credit-bearing account lost waiting meaning or credits", got)
			}
		}
	}
}

func TestDashboardUsageAggregateWindowsOnly(t *testing.T) {
	for _, compact := range []bool{false, true} {
		model := usageFixture(t, "model-scoped")
		rows := model.usagePanel(54, 17, compact)
		got := ansi.Strip(strings.Join(rows, "\n"))
		wantRows := 5
		if compact {
			wantRows = 4
		}
		if len(rows) != wantRows || strings.Count(got, "Weekly") != 1 || strings.Contains(got, "Sol Weekly") || strings.Contains(got, "5h") {
			t.Fatal("model-scoped copy became another aggregate allowance", got)
		}
	}
}
