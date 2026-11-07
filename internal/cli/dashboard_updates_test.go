package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

func TestDashboardUpdatesMissingStoreNeverCreatesOrPolls(t *testing.T) {
	state := filepath.Join(t.TempDir(), "absent")
	config := filepath.Join(t.TempDir(), "absent")
	t.Setenv("MESH_CONFIG_DIR", config)
	var calls atomic.Int32
	watcher := newDashboardUpdateWatcher(state, "no-identity", updateCallFunc(func(context.Context, update.Host, string, any, any) error { calls.Add(1); return nil }))
	watcher.interval = time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	count := 0
	if err := watcher.Run(ctx, func(view DashboardUpdates) {
		count++
		if len(view.Sources) != 0 || view.Problem != "" {
			t.Errorf("absence became status: %+v", view)
		}
		if count == 3 {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("unenrolled dashboard contacted coordinator")
	}
	for _, path := range []string{state, config} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created %s: %v", path, err)
		}
	}
}
func dashboardGrantFixture(t *testing.T) (string, update.Host, update.Host, *update.Store) {
	t.Helper()
	state, local := setupUpdateCLI(t)
	peer, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	coordinator := update.Host{ID: peer.ID, Endpoint: "ws://127.0.0.1:1/pinned", Platform: release.CurrentPlatform()}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	fleet := update.Fleet{Version: 1, Name: "fixture", Revision: 1, Members: []update.Host{local, coordinator}}
	saveUpdateTestFleet(t, filepath.Join(filepath.Dir(path), "fleet.json"), fleet)
	store, err := update.OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	return state, local, coordinator, store
}
func TestDashboardUpdatesConfigOnlyPinsExactCoordinatorAndBothGroups(t *testing.T) {
	for _, domain := range []bool{false, true} {
		t.Run(fmt.Sprint(domain), func(t *testing.T) {
			state, local, coordinator, store := dashboardGrantFixture(t)
			if _, err := store.SetConfigurationGrant(t.Context(), 0, local.ID, coordinator.ID, !domain, domain); err != nil {
				t.Fatal(err)
			}
			if err := SaveHost(HostRecord{ID: coordinator.ID, MeshIdentity: coordinator.ID, Endpoint: "ws://127.0.0.1:2/other"}); err != nil {
				t.Fatal(err)
			}
			var calls int
			caller := updateCallFunc(func(ctx context.Context, host update.Host, action string, _ any, output any) error {
				calls++
				if !reflect.DeepEqual(host, coordinator) || action != "configuration.status" {
					t.Fatalf("substituted authority: %+v %s", host, action)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > dashboardUpdateDeadline {
					t.Fatal("status lacks bounded deadline")
				}
				*output.(*update.ConfigurationStatus) = update.ConfigurationStatus{Revision: 9}
				return nil
			})
			watcher := newDashboardUpdateWatcher(state, local.ID, caller)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if err := watcher.Run(ctx, func(view DashboardUpdates) {
				cancel()
				if len(view.Sources) != 1 || view.Sources[0].Automatic != nil || view.Sources[0].Configuration.Value.Revision != 9 {
					t.Fatalf("lost config-only status: %+v", view)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("unexpected calls %d", calls)
			}
			if _, err := store.SetConfigurationGrant(t.Context(), 1, local.ID, "", false, false); err != nil {
				t.Fatal(err)
			}
			selection, err := readDashboardUpdateSelection(t.Context(), state, local.ID)
			if err != nil || len(selection.Bindings) != 0 {
				t.Fatalf("revoked grant polls: %+v %v", selection, err)
			}
		})
	}
}

func TestDashboardUpdatesDifferentBinaryAndConfigurationCoordinators(t *testing.T) {
	state, local, coordinator, store := dashboardGrantFixture(t)
	path, _ := ConfigPath()
	fleet, err := update.ReadFleet(filepath.Join(filepath.Dir(path), "fleet.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecipientConsent(t.Context(), 0, update.ConsentChange{LocalHostID: local.ID, CoordinatorID: coordinator.ID, Fleet: fleet, Epoch: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetConfigurationGrant(t.Context(), 0, local.ID, local.ID, true, false); err != nil {
		t.Fatal(err)
	}
	selection, err := readDashboardUpdateSelection(t.Context(), state, local.ID)
	if err != nil || len(selection.Bindings) != 2 {
		t.Fatalf("collapsed independent authorities: %+v %v", selection, err)
	}
	if !selection.Bindings[0].Automatic || selection.Bindings[0].Configuration || selection.Bindings[0].Host.ID != coordinator.ID {
		t.Fatal(selection)
	}
	if selection.Bindings[1].Automatic || !selection.Bindings[1].Configuration || selection.Bindings[1].Host.ID != local.ID {
		t.Fatal(selection)
	}
}
func TestDashboardUpdatesRejectsChangedFleetOrLocalBinding(t *testing.T) {
	state, local, coordinator, store := dashboardGrantFixture(t)
	if _, err := store.SetConfigurationGrant(t.Context(), 0, local.ID, coordinator.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := readDashboardUpdateSelection(t.Context(), state, coordinator.ID); err == nil {
		t.Fatal("adopted someone else's local grant")
	}
	path, _ := ConfigPath()
	saveUpdateTestFleet(t, filepath.Join(filepath.Dir(path), "fleet.json"), update.Fleet{Version: 1, Name: "changed", Revision: 2, Members: []update.Host{local}})
	if _, err := readDashboardUpdateSelection(t.Context(), state, local.ID); err == nil {
		t.Fatal("searched address book for missing coordinator")
	}
}
func TestDashboardUpdatesWithdrawnSelectionAndCancellationJoin(t *testing.T) {
	binding := dashboardUpdateBinding{Host: update.Host{ID: "pinned"}, Automatic: true}
	selection := dashboardUpdateSelection{Bindings: []dashboardUpdateBinding{binding}, Fingerprint: [32]byte{1}}
	var withdrawn atomic.Bool
	var stopped atomic.Bool
	watcher := &dashboardUpdateWatcher{interval: time.Millisecond, read: func(context.Context) (dashboardUpdateSelection, error) {
		if withdrawn.Load() {
			return dashboardUpdateSelection{}, nil
		}
		return selection, nil
	}, caller: updateCallFunc(func(_ context.Context, _ update.Host, _ string, _, output any) error {
		withdrawn.Store(true)
		*output.(*update.AutomaticStatus) = update.AutomaticStatus{State: "old authority"}
		return nil
	})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := watcher.Run(ctx, func(view DashboardUpdates) {
		if len(view.Sources) > 0 {
			t.Error("late withdrawn response published")
		}
		cancel()
	}); err != nil {
		t.Fatal(err)
	}
	withdrawn.Store(false)
	entered := make(chan struct{})
	watcher.caller = updateCallFunc(func(ctx context.Context, _ update.Host, _ string, _, _ any) error {
		close(entered)
		<-ctx.Done()
		stopped.Store(true)
		return fmt.Errorf("status cancelled: %w", ctx.Err())
	})
	ctx, cancel = context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx, func(DashboardUpdates) { t.Error("cancelled response published") }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("watcher did not join")
		}
	})
	<-entered
	cancel()
	select {
	case err := <-done:
		if err != nil || !stopped.Load() {
			t.Fatal(err)
		}
		done <- nil
	case <-time.After(time.Second):
		t.Fatal("owned request did not join")
	}
}
func TestDashboardUpdatesReadRefusalIsNotHostUnreachable(t *testing.T) {
	for _, cause := range []error{&update.RemoteError{Problem: "administrator admission absent"}, errors.New("connection refused")} {
		observation, _ := dashboardStatusCall(t.Context(), updateCallFunc(func(context.Context, update.Host, string, any, any) error { return cause }), update.Host{}, "automatic.status", nil, nil)
		if observation.State == DashboardStatusObserved {
			t.Fatal(observation)
		}
		var remote *update.RemoteError
		if errors.As(cause, &remote) && observation.State != DashboardStatusRefused {
			t.Fatal(observation)
		}
		rows := statusUnavailableRow("host", "Automatic update", observation, time.Now())
		if len(rows) != 1 || strings.Contains(rows[0].Text, "unreachable") || strings.Contains(rows[0].Text, cause.Error()) {
			t.Fatal(rows)
		}
	}
}
func TestDashboardUpdateProjectionPreservesCompleteValuesAndAges(t *testing.T) {
	now := time.Now().UTC()
	upstream := now.Add(-time.Hour)
	wanted := update.FeedValue{Present: true, URL: "https://feed.mesh.example.test/usage"}
	domain := update.DomainValue{Present: true}
	target := update.ConfigurationTarget{HostID: "recipient", State: "saved", Receipt: &update.ConfigurationReceipt{Revision: 1, State: "saved", FeedState: "saved", Domain: &update.DomainReceipt{State: "saved", Desired: domain}}, Daemon: &update.DomainObservation{Digest: "old", InstanceID: "actual", ObservedAt: now}, Observers: []update.ConfigurationObservation{{HostID: "recipient", Generation: 4, Digest: wanted.Digest(), Enabled: true, State: "enabled", Failing: true, HTTPAt: now, PublishedAt: now.Add(-time.Minute), UpstreamAt: &upstream, RenewedAt: now, DomainDigest: "old"}}}
	auto := update.AutomaticStatus{State: "stopped", Record: &update.AutomaticRecord{Stop: &update.AutomaticStop{Kind: "direct_transition_blocked", HostID: "recipient"}, Canary: &update.Canary{HostID: "recipient", State: "failed"}, LastCompleted: &update.CompletedFleetRelease{Version: "v0.1.0", At: upstream}, Current: &update.Run{Release: release.Manifest{Version: "v0.2.0"}, UpdatedAt: upstream, Targets: []update.Target{{Host: update.Host{ID: "recipient"}, State: update.Offline, Build: &release.Build{Version: "v0.1.0"}}}}}}
	config := update.ConfigurationStatus{Change: &update.ConfigurationChange{Proposal: update.ConfigurationProposal{Recipients: []update.RecipientConfiguration{{Host: update.Host{ID: "recipient"}, Feed: &update.FeedChange{URL: wanted.URL}, Domain: &update.DomainChange{}}}}, Targets: []update.ConfigurationTarget{target}}}
	observation := DashboardStatusObservation{State: DashboardStatusObserved, At: now}
	view := DashboardUpdates{Generation: 5, Sources: []DashboardUpdateSource{{Automatic: &DashboardStatus[update.AutomaticStatus]{observation, &auto}, Configuration: &DashboardStatus[update.ConfigurationStatus]{observation, &config}}}}
	before, _ := json.Marshal(view)
	rows := DashboardUpdateRows(view, now)
	after, _ := json.Marshal(view)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("presentation rewrote authority or original ages")
	}
	text := fmt.Sprint(rows)
	for _, expected := range []string{"v0.2.0", "last observed daemon v0.1.0", "Last successful fleet release v0.1.0", "exact pinned release", "Canary failed", "manual bridge", "daemon restart required", "dashboard reopen required", "HTTPS verification unknown", "transport or parse failed", "Upstream observations stale", upstream.Format(time.RFC3339)} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q in %s", expected, text)
		}
	}
	rows = DashboardUpdateRows(view, now.Add(time.Minute))
	if strings.Contains(fmt.Sprint(rows), "Feed loaded") {
		t.Fatal("expired status remains current")
	}
}
