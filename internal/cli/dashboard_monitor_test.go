package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestDashboardMonitorAdmissionAndJoinedCancellation(t *testing.T) {
	var active, peak, dials atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	dial := func(ctx context.Context, _ HostRecord) (transport.Conn, error) {
		current := active.Add(1)
		for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
		}
		defer active.Add(-1)
		dials.Add(1)
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil, errors.New("fixture offline")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	monitor := dashboardMonitor{watcher: NewStateWatcher(dial)}
	for i := range 8 {
		monitor.records = append(monitor.records, HostRecord{ID: fmt.Sprint(i), Alias: fmt.Sprint(i)})
	}
	failed := map[string]bool{}
	publications := 0
	err := monitor.Run(ctx, func(view DashboardHostView) {
		publications++
		if view.Connection == StateUnreachable {
			failed[view.Host.ID] = true
		}
		if len(failed) == len(monitor.records) {
			cancel()
		}
	})
	if err != nil || len(failed) != 8 || peak.Load() > 4 || active.Load() != 0 || dials.Load() != 8 {
		t.Fatalf("admission/join: err=%v hosts=%d peak=%d active=%d dials=%d", err, len(failed), peak.Load(), active.Load(), dials.Load())
	}
	if publications != 16 {
		t.Fatalf("unexpected publications including late results: %d", publications)
	}
}

func TestDashboardMonitorBackpressureStopsAllReaders(t *testing.T) {
	host := HostRecord{ID: "host", Alias: "pc", MeshIdentity: "identity"}
	var closed atomic.Int32
	dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
		if request.Type == protocol.TypeStateWatch {
			return reviewSnapshot(host)
		}
		return nil
	})
	watcher := NewStateWatcher(func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		conn, err := dial(ctx, host)
		return &dashboardCountedConn{Conn: conn, closed: &closed}, err
	})
	monitor := dashboardMonitor{watcher: watcher, records: []HostRecord{host}}
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	exited := make(chan error, 1)
	go func() {
		exited <- monitor.Run(ctx, func(view DashboardHostView) {
			if view.Connection == StateReachable {
				close(ready)
				<-ctx.Done()
			}
		})
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("stream did not publish")
	}
	cancel()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked publisher kept reader alive")
	}
	if closed.Load() == 0 {
		t.Fatal("watch connection remained open")
	}
}

type dashboardCountedConn struct {
	transport.Conn
	closed *atomic.Int32
}

func (c *dashboardCountedConn) Close() error {
	c.closed.Add(1)
	if err := c.Conn.Close(); err != nil {
		return fmt.Errorf("fixture close: %w", err)
	}
	return nil
}

func TestDashboardReadDeadlineRetainsReachabilityAndCancels(t *testing.T) {
	host := HostRecord{ID: "host", Alias: "pc", MeshIdentity: "identity"}
	watcher := NewStateWatcher(reviewControlDial(t, func(_ HostRecord, _ protocol.Control) *protocol.Control { return nil }))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	start := time.Now()
	var seen StateView
	err := watcher.Watch(ctx, host, protocol.StateWatch{Topics: []string{protocol.TopicMetrics}}, func(view StateView) { seen = view; cancel() })
	if !errors.Is(err, context.Canceled) || seen.Connection != StateReachable || seen.Problem == "" || !seen.Sections[protocol.TopicMetrics].Observation.Failing || time.Since(start) > 2*time.Second {
		t.Fatalf("deadline/verified reachability: %v %+v elapsed=%v", err, seen, time.Since(start))
	}
}

func TestDashboardInventoryDeduplicatesLocalAndUsesLocalSocket(t *testing.T) {
	fixture := setupCommandTestHost(t)
	stateDir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	local, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	localRecord := HostRecord{Alias: "adopted-local", ID: local.ID, MeshIdentity: local.ID, Endpoint: fixture.host.Endpoint}
	if err := SaveHost(localRecord); err != nil {
		t.Fatal(err)
	}
	records, localID, socket, err := dashboardInventory()
	if err != nil || localID != local.ID || len(records) != 2 || records[0].Alias != "adopted-local" {
		t.Fatalf("inventory: %v %s %+v", err, localID, records)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // fixture teardown
	accepted := make(chan struct{})
	go func() {
		stream, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = stream.Close()
			close(accepted)
		}
	}()
	var remote atomic.Int32
	dial := dashboardControlDialer(localID, socket, func(context.Context, HostRecord) (transport.Conn, error) {
		remote.Add(1)
		return nil, errors.New("remote fixture")
	})
	conn, err := dial(t.Context(), localRecord)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("local daemon socket unused")
	}
	if remote.Load() != 0 || socket != filepath.Join(stateDir, "daemon.sock") {
		t.Fatal("local host reached remote dialer")
	}
}

func TestDashboardOfflineCacheRetainedAndIdentityScoped(t *testing.T) {
	fixture := setupCommandTestHost(t)
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close() //nolint:errcheck // fixture teardown
	if err := cache.Save(t.Context(), fixture.host, []protocol.SessionInfo{reviewSession(fixture.host)}); err != nil {
		t.Fatal(err)
	}
	monitor := dashboardMonitor{cache: cache}
	cached := monitor.cachedView(t.Context(), fixture.host)
	if cached.Sessions.Total != 1 || !cached.Sessions.Failing || !cached.Sessions.Stale(time.Now(), cached.LastReply) {
		t.Fatal("cache became a live observation", cached)
	}
	changed := fixture.host
	changed.MeshIdentity = "replacement-identity"
	if view := monitor.cachedView(t.Context(), changed); view.Sessions.Total != 0 {
		t.Fatal("cache crossed identity generation")
	}
	monitor.records = []HostRecord{fixture.host}
	monitor.watcher = NewStateWatcher(func(context.Context, HostRecord) (transport.Conn, error) { return nil, errors.New("offline fixture") })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var final DashboardHostView
	if err := monitor.Run(ctx, func(view DashboardHostView) {
		final = view
		if view.Connection == StateUnreachable {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if final.Sessions.Total != 1 || len(final.Sessions.Rows) != 1 {
		t.Fatal("failed read discarded offline cache")
	}
}

func TestDashboardPickerStillWaitsForFirstCatalog(t *testing.T) {
	host := HostRecord{ID: "host", Alias: "pc", MeshIdentity: "identity"}
	var requested sync.Once
	subscribed := make(chan struct{})
	release := make(chan struct{})
	dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
		if request.Type != protocol.TypeStateWatch {
			return nil
		}
		requested.Do(func() { close(subscribed) })
		<-release
		return reviewSnapshot(host)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	picker := newPickerState(ctx, dial)
	defer picker.close()
	ready, _ := picker.selectHost(host)
	select {
	case <-subscribed:
	case <-time.After(time.Second):
		t.Fatal("watch did not subscribe")
	}
	select {
	case <-ready:
		t.Fatal("verified identity published an empty first catalog")
	default:
	}
	close(release)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("first snapshot did not release picker")
	}
	rows, err := picker.read(ctx, host)
	if err != nil || len(rows.Sessions) != 1 {
		t.Fatalf("picker initial rows: %+v %v", rows, err)
	}
}
