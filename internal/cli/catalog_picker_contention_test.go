package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func parentReviewPickerCache(t *testing.T) (*SQLiteCatalogCache, HostRecord, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache, HostRecord{ID: "host-review", MachineName: "review", MeshIdentity: "review"}, state
}

func TestCatalogReviewPickerSessionConvergenceAfterSkippedWrite(t *testing.T) {
	cache, host, state := parentReviewPickerCache(t)
	old := protocol.SessionInfo{ID: "OLD1", HostID: host.ID, Command: []string{"sh"}, State: "detached", CreatedAt: time.Now().UTC()}
	if _, err := cache.Save(t.Context(), host, []protocol.SessionInfo{old}); err != nil {
		t.Fatal(err)
	}
	next := old
	next.ID = "NEW1"
	view := StateView{initialized: true, Sessions: []protocol.SessionInfo{next}}
	picker := &pickerState{cache: cache}
	release := holdCatalogWriter(t, state)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	start := time.Now()
	cached, err := picker.saveCache(ctx, host, view, []protocol.SessionInfo{old}, nil)
	if err != nil || ctx.Err() != nil {
		t.Fatalf("skip error=%v context=%v", err, ctx.Err())
	}
	t.Logf("held writer skipped at %v with live context", time.Since(start))
	if time.Since(start) < 4800*time.Millisecond {
		t.Fatal("did not exhaust admission budget")
	}
	release()
	_, err = picker.saveCache(ctx, host, view, cached, err)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := cache.Load(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("unchanged later picker observation did not repair skipped write: %#v", rows)
	}
	if _, err := cache.Save(ctx, host, view.Sessions); err != nil {
		t.Fatal(err)
	}
	rows, err = cache.Load(t.Context(), host)
	if err != nil || len(rows) != 2 {
		t.Fatalf("direct save positive control failed: rows=%#v err=%v", rows, err)
	}
	t.Log("same live observation commits through direct Save once memoization is bypassed")
}

func TestCatalogReviewPickerServiceConvergenceAfterSkippedWrite(t *testing.T) {
	cache, host, state := parentReviewPickerCache(t)
	old := protocol.ServiceInfo{Name: "old", Kind: "proxy", Target: "3000"}
	if _, err := cache.SaveServices(t.Context(), host, "", []protocol.ServiceInfo{old}); err != nil {
		t.Fatal(err)
	}
	view := StateView{Services: []protocol.ServiceInfo{{Name: "new", Kind: "proxy", Target: "4000"}}}
	monitor := &pickerServiceMonitor{cache: cache}
	var saved pickerSavedCatalog
	release := holdCatalogWriter(t, state)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	start := time.Now()
	if err := monitor.saveCatalog(ctx, host, view, false, &saved); err != nil || ctx.Err() != nil {
		t.Fatalf("skip error=%v context=%v", err, ctx.Err())
	}
	t.Logf("held writer skipped at %v with live context", time.Since(start))
	if time.Since(start) < 4800*time.Millisecond {
		t.Fatal("did not exhaust admission budget")
	}
	release()
	if err := monitor.saveCatalog(ctx, host, view, false, &saved); err != nil {
		t.Fatal(err)
	}
	rows, err := cache.LoadServices(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Service.Name != "new" {
		t.Errorf("unchanged later picker observation did not repair skipped write: %#v", rows)
	}
	if _, err := cache.SaveServices(ctx, host, "", view.Services); err != nil {
		t.Fatal(err)
	}
	rows, err = cache.LoadServices(t.Context(), host)
	if err != nil || len(rows) != 1 || rows[0].Service.Name != "new" {
		t.Fatalf("direct save positive control failed: rows=%#v err=%v", rows, err)
	}
	t.Log("same live observation commits through direct SaveServices once memoization is bypassed")
}

func TestCatalogReviewPickerLiveSnapshotNotBlockedByCache(t *testing.T) {
	cache, host, stateDir := parentReviewPickerCache(t)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	old := reviewSession(host)
	old.ID = "OLD1"
	if _, err := cache.Save(t.Context(), host, []protocol.SessionInfo{old}); err != nil {
		t.Fatal(err)
	}
	release := holdCatalogWriter(t, stateDir)
	life, stop := context.WithCancel(t.Context())
	dial := reviewControlDial(t, func(h HostRecord, r protocol.Control) *protocol.Control {
		if r.Type == protocol.TypeStateWatch {
			return reviewSnapshot(h)
		}
		return nil
	})
	entered := make(chan struct{}, 1)
	observed := catalogCacheFuncs{load: cache.Load, save: func(ctx context.Context, h HostRecord, rows []protocol.SessionInfo) (bool, error) {
		entered <- struct{}{}
		return cache.Save(ctx, h, rows)
	}}
	picker := newPickerState(life, dial)
	picker.cache = observed
	defer func() { release(); stop(); picker.close() }()
	picker.selectHost(host)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("live snapshot never reached cache write")
	}
	short, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	rows, err := picker.read(short, host)
	cancel()
	if err != nil || len(rows.Sessions) != 1 || rows.Sessions[0].ID != "7K3D" {
		t.Errorf("live watch snapshot is hidden behind advisory writer admission: rows=%#v error=%v", rows, err)
	}
	release()
	follow, cancelFollow := context.WithTimeout(t.Context(), time.Second)
	defer cancelFollow()
	rows, err = picker.read(follow, host)
	if err != nil || len(rows.Sessions) != 1 || rows.Sessions[0].ID != "7K3D" {
		t.Fatalf("live snapshot positive control after unlock: rows=%#v error=%v", rows, err)
	}
	t.Log("same already-received live snapshot becomes readable immediately after writer is released")
}

func TestCatalogReviewServiceLivePublicationDoesNotWaitForCache(t *testing.T) {
	cache, host, state := parentReviewPickerCache(t)
	release := holdCatalogWriter(t, state)
	t.Cleanup(release)
	dial := reviewControlDial(t, func(_ HostRecord, request protocol.Control) *protocol.Control {
		if request.Type != protocol.TypeStateWatch {
			return nil
		}
		return &protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Services: []protocol.ServiceInfo{{Name: "live", Kind: "proxy", Target: "3000"}}, Current: map[string]protocol.Observation{protocol.TopicServices: {}}}}
	})
	monitor := &pickerServiceMonitor{cache: cache, watcher: NewStateWatcher(dial)}
	life, stop := context.WithCancel(t.Context())
	updates := make(chan PickerServicesUpdate, 2)
	done := make(chan struct{})
	go func() { defer close(done); monitor.watchHost(life, host, updates) }()
	t.Cleanup(func() { stop(); <-done })
	deadline := time.NewTimer(1500 * time.Millisecond)
	defer deadline.Stop()
	found := false
	for !found {
		select {
		case update := <-updates:
			found = !update.Catalog.Stale && len(update.Catalog.Rows) == 1 && update.Catalog.Rows[0].Service.Name == "live"
		case <-deadline.C:
			t.Fatal("live service observation waited for advisory write admission")
		}
	}
}

func TestCatalogReviewServiceCacheFailureRemainsVisible(t *testing.T) {
	host := HostRecord{ID: "host-failure", MeshIdentity: "peer"}
	cache := &pickerServiceCacheStub{saveErr: errors.New("fixture cache failure")}
	dial := reviewControlDial(t, func(_ HostRecord, request protocol.Control) *protocol.Control {
		if request.Type != protocol.TypeStateWatch {
			return nil
		}
		return &protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Services: []protocol.ServiceInfo{{Name: "live", Kind: "proxy", Target: "3000"}}, Current: map[string]protocol.Observation{protocol.TopicServices: {}}}}
	})
	monitor := &pickerServiceMonitor{hosts: []HostRecord{host}, cache: cache, watcher: NewStateWatcher(dial)}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	sawLive, sawProblem := false, false
	err := monitor.Run(ctx, func(update PickerServicesUpdate) {
		if !update.Catalog.Stale && len(update.Catalog.Rows) == 1 {
			sawLive = true
		}
		if strings.Contains(update.Problem, "fixture cache failure") {
			sawProblem = true
			cancel()
		}
	})
	if err != nil || !sawLive || !sawProblem {
		t.Fatalf("live=%v cache diagnostic=%v error=%v", sawLive, sawProblem, err)
	}
}
