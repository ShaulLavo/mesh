package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
)

func TestDashboardPassiveViewFitsAndFreshnessIsIndependent(t *testing.T) {
	now := pickerTestNow
	input := cli.DashboardInput{Hosts: []cli.DashboardHost{{ID: "pc", Alias: "pc"}}, Wall: true}
	model := newDashboard(input, now)
	model.receive(cli.DashboardHostView{Host: input.Hosts[0], Connection: cli.StateReachable, LastReply: now,
		CPU:      cli.DashboardMeasurement[float64]{State: "available", Value: 0, Sample: "cpu", MeasuredAt: now},
		RAM:      cli.DashboardMeasurement[cli.DashboardMemory]{State: "available", Value: cli.DashboardMemory{TotalBytes: 16 << 30, AvailableBytes: 8 << 30, Estimate: "Linux MemAvailable estimate"}, Sample: "ram", MeasuredAt: now.Add(-20 * time.Second)},
		Sessions: cli.DashboardCatalog[cli.DashboardSession]{Total: 30, ObservedAt: now},
		Services: cli.DashboardCatalog[cli.DashboardService]{Total: 2, ObservedAt: now, Failing: true},
	})
	view := ansi.Strip(model.View().Content)
	assertFits(t, view, 80, 24)
	for _, value := range []string{"CPU 0%", "RAM 8.0/16.0 GiB stale 20s", "sessions 30 live", "services 2", "cached 2"} {
		if !strings.Contains(view, value) {
			t.Fatalf("missing %q: %s", value, view)
		}
	}
	for _, value := range []string{"busy", "WORKING", "enter", "↑", "↓", "navigate"} {
		if strings.Contains(view, value) {
			t.Fatalf("passive view gained %q", value)
		}
	}
	unchanged := model.View().Content
	updated, command := model.Update(runeKey('n'))
	if command != nil || updated.(dashboardModel).View().Content != unchanged {
		t.Fatal("key changed passive screen")
	}
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 50, Height: 12})
	resized := updated.(dashboardModel).View().Content
	if !strings.Contains(resized, "needs 80×24") {
		t.Fatal("small terminal has no resize message")
	}
	assertFits(t, resized, 50, 12)
}

func TestDashboardCurrentKeepsQuietCatalogLiveAndReportsOverflow(t *testing.T) {
	now := pickerTestNow
	model := newDashboard(cli.DashboardInput{}, now)
	for i := range 8 {
		model.hosts = append(model.hosts, cli.DashboardHostView{Host: cli.DashboardHost{ID: string(rune('a' + i)), Alias: "long alias " + strings.Repeat("x", 120)}, Connection: cli.StateReachable, LastReply: now, Sessions: cli.DashboardCatalog[cli.DashboardSession]{ObservedAt: now, Total: 1}, Services: cli.DashboardCatalog[cli.DashboardService]{ObservedAt: now}})
	}
	view := ansi.Strip(model.render())
	assertFits(t, view, 80, 24)
	if !strings.Contains(view, "Hosts 8 / 8 visible · 0 omitted") {
		t.Fatal("overflow silently hid hosts", view)
	}
	old := model.hosts[0]
	model.now = now.Add(5 * time.Minute)
	old.LastReply = model.now
	old.Sessions.ObservedAt = model.now
	model.receive(old)
	if !strings.Contains(model.render(), "sessions 1 live") {
		t.Fatal("current confirmation did not renew quiet catalog")
	}
}

func TestDashboardNoInputCancellationJoinsWatch(t *testing.T) {
	var output dashboardTestOutput
	input := cli.DashboardInput{Hosts: []cli.DashboardHost{{ID: "pc", Alias: "pc"}}}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	input.Watch = func(ctx context.Context, publish func(cli.DashboardHostView)) error {
		defer close(finished)
		publish(cli.DashboardHostView{Host: input.Hosts[0], Connection: cli.StateReachable})
		cancel()
		<-ctx.Done()
		return nil
	}
	if err := runDashboard(ctx, input, &output, tea.WithWindowSize(80, 24)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("watch outlived terminal")
	}
	// A real PTY integration checks the emitted alternate-screen exit too.
}

type dashboardTestOutput struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (w *dashboardTestOutput) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.data.Write(value)
	if err != nil {
		return n, fmt.Errorf("fixture output: %w", err)
	}
	return n, nil
}

func TestDashboardHistoryDistinctSamplesGapsResetsAndBounds(t *testing.T) {
	now := pickerTestNow
	metric := cli.DashboardMeasurement[float64]{State: "available", Value: 50, Sample: "one", Segment: 1, MeasuredAt: now.Add(-100 * time.Second)}
	points := dashboardRemember(nil, metric, metric.MeasuredAt)
	metric.MeasuredAt = now.Add(-99 * time.Second)
	points = dashboardRemember(points, metric, metric.MeasuredAt)
	if len(points) != 1 {
		t.Fatal("same sample renewed history")
	}
	metric.Sample, metric.Segment, metric.MeasuredAt = "two", 2, now.Add(-50*time.Second)
	points = dashboardRemember(points, metric, metric.MeasuredAt)
	if _, found := dashboardGraphValue(points, now.Add(-95*time.Second)); found {
		t.Fatal("gap filled with invented data")
	}
	if value, found := dashboardGraphValue(points, now.Add(-99*time.Second)); !found || value != 50 {
		t.Fatal("old segment discarded")
	}
	metric.Failing = true
	metric.Sample = "failed"
	if len(dashboardRemember(points, metric, now)) != 2 {
		t.Fatal("failed sample graphed")
	}
	metric.Failing = false
	for i := range 100 {
		metric.Sample = fmt.Sprint(i)
		metric.MeasuredAt = now.Add(time.Duration(i-100) * time.Second)
		points = dashboardRemember(points, metric, metric.MeasuredAt)
	}
	if len(points) > 64 {
		t.Fatal("unbounded history")
	}
	if len(dashboardPrune(points, now.Add(130*time.Second))) != 0 {
		t.Fatal("history did not expire")
	}
	if dashboardAreaCell(0, true, 1, 4, false) == dashboardAreaCell(0, false, 1, 4, false) {
		t.Fatal("valid zero looks like missing data")
	}
}

func TestDashboardWallContainsHistoriesTemperatureUptimeAndBoundedSummaries(t *testing.T) {
	now := pickerTestNow
	model := newDashboard(cli.DashboardInput{Wall: true}, now)
	model.width, model.height = 160, 48
	for i := range 6 {
		id := fmt.Sprint(i)
		host := cli.DashboardHostView{Host: cli.DashboardHost{ID: id, Alias: "pc-" + id}, Connection: cli.StateReachable, LastReply: now,
			CPU:         cli.DashboardMeasurement[float64]{State: "available", Value: 25, Sample: "cpu", MeasuredAt: now},
			RAM:         cli.DashboardMeasurement[cli.DashboardMemory]{State: "available", Value: cli.DashboardMemory{TotalBytes: 16 << 30, AvailableBytes: 8 << 30, Estimate: "Linux MemAvailable estimate"}, Sample: "ram", MeasuredAt: now},
			Temperature: cli.DashboardMeasurement[cli.DashboardTemperature]{State: "available", Value: cli.DashboardTemperature{Sensor: "package", Celsius: 42}, Sample: "temp", MeasuredAt: now},
			Uptime:      cli.DashboardMeasurement[uint64]{State: "available", Value: 3600, Sample: "uptime", MeasuredAt: now},
			Sessions:    cli.DashboardCatalog[cli.DashboardSession]{Rows: []cli.DashboardSession{{ID: "7K3D", State: "running", Command: "quiet shell"}}, Total: 30, ObservedAt: now},
			Services:    cli.DashboardCatalog[cli.DashboardService]{Rows: []cli.DashboardService{{Name: "website", State: "healthy"}, {Name: "broken", State: "unhealthy", Problem: "fixture failure", Failed: true}}, Total: 20, ObservedAt: now},
		}
		model.hosts = append(model.hosts, cli.DashboardHostView{Host: host.Host})
		model.receive(host)
	}
	view := ansi.Strip(model.render())
	assertFits(t, view, 160, 48)
	for _, label := range []string{"CPU ", "25%", "RAM 8.0 / 16.0 GiB", "CPU 42°", "up 1h", "120s", "0–100%", "quiet shell", "fixture failure", "180 total", "120 total", "Hosts 6 / 6 visible"} {
		if !strings.Contains(view, label) {
			t.Fatalf("missing wall %q: %s", label, view)
		}
	}
	model.hosts[0].Connection = cli.StateUnreachable
	if len(model.history["0"].cpu) != 1 {
		t.Fatal("offline host lost history")
	}
	model.width, model.height = 80, 24
	assertFits(t, model.render(), 80, 24)
	if !strings.Contains(model.render(), "Hosts 6 / 6 visible") {
		t.Fatal("compact table omitted fleet")
	}
}
