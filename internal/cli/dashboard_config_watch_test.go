package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/usagefeed"
)

type controlledDashboardFeed struct {
	name         string
	active, peak *atomic.Int32
	stopped      chan struct{}
}

func (f controlledDashboardFeed) Run(ctx context.Context, publish func(usagefeed.Result)) error {
	active := f.active.Add(1)
	defer f.active.Add(-1)
	defer close(f.stopped)
	for old := f.peak.Load(); active > old; old = f.peak.Load() {
		if f.peak.CompareAndSwap(old, active) {
			break
		}
	}
	observed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	publish(usagefeed.Result{Revision: 1, Snapshot: &usagefeed.Snapshot{GeneratedAt: observed, Accounts: []usagefeed.Account{{ID: f.name, LastSeenAt: &observed}}}})
	<-ctx.Done()
	publish(usagefeed.Result{Revision: 2, Snapshot: &usagefeed.Snapshot{Accounts: []usagefeed.Account{{ID: "late-" + f.name}}}})
	return fmt.Errorf("fixture reader stopped: %w", ctx.Err())
}
func TestDashboardFeedWatcherGenerationHeartbeatAndJoinedReplacement(t *testing.T) {
	var mu sync.Mutex
	value := update.FeedValue{}
	var readErr error
	var active, peak atomic.Int32
	stopped := make(chan string, 4)
	observations := make(chan update.ConfigurationObservation, 32)
	w := &dashboardFeedWatcher{interval: 5 * time.Millisecond, heartbeat: 20 * time.Millisecond, hostID: "fixture-host", instanceID: "fixture-instance", acknowledgements: make(chan DashboardUsageAcknowledgement, 1)}
	w.read = func() (update.FeedValue, error) { mu.Lock(); defer mu.Unlock(); return value, readErr }
	w.fetcher = func(name string) (dashboardFeedRunner, error) {
		done := make(chan struct{})
		t.Cleanup(func() { assertDashboardFetcherJoined(t, done, name) })
		return controlledDashboardFeed{name, &active, &peak, done}, nil
	}
	w.observe = func(_ context.Context, o update.ConfigurationObservation) error { observations <- o; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	events := make(chan DashboardUsageUpdate, 32)
	go func() { done <- w.Run(ctx, func(event DashboardUsageUpdate) { events <- event }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("watcher failed to join")
		}
	})
	first := nextDashboardEvent(t, events, func(e DashboardUsageUpdate) bool { return e.State == dashboardFeedDisabled })
	if first.Generation != 1 || first.Enabled {
		t.Fatal(first)
	}
	select {
	case <-observations:
		t.Fatal("unacknowledged event reported loaded")
	case <-time.After(25 * time.Millisecond):
	}
	w.acknowledge(first.Acknowledgement())
	_ = nextDashboardObservation(t, observations)
	change := func(next update.FeedValue, err error) { mu.Lock(); value, readErr = next, err; mu.Unlock() }
	change(update.FeedValue{Present: true, URL: "http://fixture/A"}, nil)
	a := nextDashboardEvent(t, events, func(e DashboardUsageUpdate) bool { return e.Result.Snapshot != nil })
	if a.Result.Revision != 1 || a.Result.Snapshot.Accounts[0].ID != "http://fixture/A" {
		t.Fatal(a)
	}
	w.acknowledge(a.Acknowledgement())
	oa := nextDashboardObservation(t, observations)
	for oa.Generation != a.Generation {
		oa = nextDashboardObservation(t, observations)
	}
	renewal := nextDashboardObservation(t, observations)
	if renewal.HTTPAt != oa.HTTPAt || renewal.PublishedAt != oa.PublishedAt || renewal.FeedRevision != 1 || renewal.UpstreamAt == nil || !renewal.UpstreamAt.Equal(*oa.UpstreamAt) {
		t.Fatal("heartbeat manufactured freshness", oa, renewal)
	}
	change(update.FeedValue{Present: true, URL: "http://fixture/B"}, nil)
	b := nextDashboardEvent(t, events, func(e DashboardUsageUpdate) bool { return e.Generation > a.Generation && e.Result.Snapshot != nil })
	if b.Result.Revision != 1 || b.Result.Snapshot.Accounts[0].ID != "http://fixture/B" || b.Digest == a.Digest {
		t.Fatal("B revision one was lost", b)
	}
	w.acknowledge(a.Acknowledgement())
	change(update.FeedValue{}, errors.New("invalid fixture configuration"))
	invalid := nextDashboardEvent(t, events, func(e DashboardUsageUpdate) bool { return e.State == dashboardFeedInvalid })
	if invalid.Enabled || invalid.Result.Snapshot != nil {
		t.Fatal(invalid)
	}
	change(update.FeedValue{Present: true, URL: "http://fixture/B"}, nil)
	recovered := nextDashboardEvent(t, events, func(e DashboardUsageUpdate) bool {
		return e.Generation > invalid.Generation && e.Result.Snapshot != nil
	})
	if recovered.Result.Revision != 1 {
		t.Fatal(recovered)
	}
	change(update.FeedValue{Present: true}, nil)
	disabled := nextDashboardEvent(t, events, func(e DashboardUsageUpdate) bool {
		return e.State == dashboardFeedDisabled && e.Generation > recovered.Generation
	})
	if disabled.Result.Snapshot != nil || disabled.Enabled {
		t.Fatal(disabled)
	}
	w.acknowledge(disabled.Acknowledgement())
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		done <- err
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
	if peak.Load() != 1 || active.Load() != 0 {
		t.Fatalf("fetchers peak=%d active=%d", peak.Load(), active.Load())
	}
	close(stopped)
}
func nextDashboardEvent(t *testing.T, events <-chan DashboardUsageUpdate, match func(DashboardUsageUpdate) bool) DashboardUsageUpdate {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		var event DashboardUsageUpdate
		select {
		case event = <-events:
		case <-timer.C:
			t.Fatal("dashboard event missing")
			return DashboardUsageUpdate{}
		}
		if event.Result.Snapshot != nil && len(event.Result.Snapshot.Accounts) > 0 && event.Result.Snapshot.Accounts[0].ID == "late-http://fixture/A" {
			t.Fatal("late A escaped generation fence")
		}
		if match(event) {
			return event
		}
	}
}
func assertDashboardFetcherJoined(t *testing.T, done <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Error("fetcher did not join", name)
	}
}

func nextDashboardObservation(t *testing.T, events <-chan update.ConfigurationObservation) update.ConfigurationObservation {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("dashboard observation missing")
		return update.ConfigurationObservation{}
	}
}
func TestDashboardObserverUsesNoNewIdentityOrDaemon(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	if observer := localDashboardObserver("missing", "/missing/daemon.sock"); observer != nil {
		t.Fatal("observer created identity")
	}
}
