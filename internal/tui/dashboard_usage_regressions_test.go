package tui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/usagefeed"
)

type usageRegressionTransport struct{ data []byte }

func (r usageRegressionTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(r.data)), Header: make(http.Header)}, nil
}
func validatedUsageAccount(t *testing.T, m dashboardModel, a usagefeed.Account) usagefeed.Account {
	t.Helper()
	data, err := json.Marshal(usagefeed.Snapshot{SchemaVersion: 1, GeneratedAt: m.now, Accounts: []usagefeed.Account{a}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := usagefeed.New(usagefeed.Config{URL: "https://feed.example.test/static/v1.json", Client: &http.Client{Transport: usageRegressionTransport{data}}})
	if err != nil {
		t.Fatal(err)
	}
	result := f.Refresh(t.Context())
	if result.Err != nil {
		t.Fatalf("repro is not valid v1: %v", result.Err)
	}
	return result.Snapshot.Accounts[0]
}
func TestDashboardUsageReviewRegressions(t *testing.T) {
	t.Run("attention-wrapped-reason", func(t *testing.T) {
		for _, size := range [][2]int{{160, 45}, {80, 24}} {
			m := usageFixture(t, "normal")
			m.width, m.height = size[0], size[1]
			h := &m.hosts[0]
			h.Services.Rows[0] = cli.DashboardService{Name: "failed-service", State: "unhealthy", Failed: true, Problem: "health check refused"}
			h.Services.Failed = 1
			h.Services.Idle = 0
			if !strings.Contains(ansi.Strip(m.render()), "Attention") {
				t.Fatal("short-reason positive control failed")
			}
			h.Services.Rows[0].Problem = "health check failed: connection refused while reaching the local service"
			got := ansi.Strip(m.render())
			t.Logf("size=%v group rows=%d has Attention=%v has reason=%v", size, len(m.serviceAttention(*h, h.Services.Rows[0], 47)), strings.Contains(got, "Attention"), strings.Contains(got, "connection refused"))
			if !strings.Contains(got, "Attention") {
				t.Errorf("size=%v failed service disappears from Attention", size)
			}
		}
	})
	t.Run("independent-window-ages", func(t *testing.T) {
		m := usageFixture(t, "normal")
		a := m.usage.accounts[0]
		old := m.now.Add(-10 * time.Minute)
		a.Windows[1].LastSeenAt = &old
		got := ansi.Strip(strings.Join(m.usageAccountWindows(a, 50, false), "\n"))
		t.Logf("fresh differing age:\n%s", got)
		if !strings.Contains(got, "seen 10m") {
			t.Error("fresh Weekly independent age omitted")
		}
		old = m.now.Add(-18 * time.Minute)
		expired := m.now.Add(-time.Minute)
		a.Windows[1].LastSeenAt = &old
		a.Windows[1].ResetsAt = &expired
		got = ansi.Strip(strings.Join(m.usageAccountWindows(a, 50, false), "\n"))
		t.Logf("reset-passed differing age:\n%s", got)
		if !strings.Contains(got, "stale 18m") {
			t.Error("reset-passed Weekly independent age omitted")
		}
		compact := ansi.Strip(m.usageCompactWindow(a.Windows[1], 36, true))
		t.Logf("compact reset-passed: %s", compact)
		if !strings.Contains(compact, "stale 18m") || !strings.Contains(compact, "66%") {
			t.Error("compact reset-passed independent age and history omitted")
		}
	})
	t.Run("observed-cooldown-without-quota", func(t *testing.T) {
		m := usageFixture(t, "normal")
		a := m.usage.accounts[1].Account
		a.State = "cooldown"
		a.LastSeenAt = nil
		a.Routing = usagefeed.Routing{Mode: "rotating"}
		a.Windows = []usagefeed.Window{}
		a.Cooldown = &usagefeed.Cooldown{Reason: "unauthorized", ObservedAt: m.now, Source: "proxy-state"}
		a = validatedUsageAccount(t, m, a)
		got := ansi.Strip(m.usageIdentity(projectUsageAccount(a, true), 50, false))
		t.Log(got)
		if !strings.Contains(got, "cooldown") {
			t.Error("valid observed cooldown hidden by missing quota age")
		}
	})
	t.Run("cached-service-state", func(t *testing.T) {
		m := usageFixture(t, "normal")
		m.ascii = true
		h := m.hosts[0]
		h.Services.ObservedAt = m.now.Add(-12 * time.Minute)
		rows := m.serviceRows(h, 43)
		got := ansi.Strip(rows[1].text)
		t.Logf("cached service row: %s", got)
		if !strings.Contains(got, "cached") {
			t.Error("cached marker clipped")
		}
	})
	t.Run("compact-cached-sessions", func(t *testing.T) {
		m := usageFixture(t, "normal")
		m.width, m.height = 80, 24
		for i := range m.hosts {
			m.hosts[i].Connection = cli.StateUnreachable
		}
		got := ansi.Strip(m.render())
		t.Logf("usage enabled cached sessions:\n%s", got)
		m.usageEnabled = false
		old := ansi.Strip(m.render())
		t.Logf("no-feed positive control has E8WS=%v N8PF=%v", strings.Contains(old, "E8WS"), strings.Contains(old, "N8PF"))
		if !strings.Contains(got, "E8WS") && !strings.Contains(got, "N8PF") {
			t.Error("all cached session details omitted despite retained catalogs")
		}
	})
	t.Run("compact-used-left-legend", func(t *testing.T) {
		for _, name := range []string{"normal", "overflow"} {
			m := usageFixture(t, name)
			m.width, m.height = 80, 24
			m.usageFailing = true
			got := ansi.Strip(m.render())
			t.Logf("%s failed feed: paired percents=%v used/left=%v", name, strings.Contains(got, "20%/80%"), strings.Contains(got, "used/left"))
			if !strings.Contains(got, "used/left") {
				t.Errorf("%s compact failure erased ratio legend", name)
			}
		}
	})
	t.Run("weekly-only-window", func(t *testing.T) {
		m := usageFixture(t, "normal")
		a := m.usage.accounts[0].Account
		a.Windows = a.Windows[1:]
		a = validatedUsageAccount(t, m, a)
		got := ansi.Strip(strings.Join(m.usageAccountWindows(projectUsageAccount(a, true), 50, false), "\n"))
		t.Logf("valid Weekly-only account:\n%s", got)
		if strings.Count(got, "Weekly") != 1 {
			t.Error("observed Weekly duplicated by positional placeholder")
		}
	})
	t.Run("disabled-account", func(t *testing.T) {
		m := usageFixture(t, "normal")
		a := m.usage.accounts[1].Account
		a.State = "disabled"
		a = validatedUsageAccount(t, m, a)
		got := ansi.Strip(m.usageIdentity(projectUsageAccount(a, true), 50, false))
		t.Logf("disabled account identity: %s", got)
		if !strings.Contains(got, "disabled") {
			t.Error("disabled credential state has no presentation")
		}
	})
}
