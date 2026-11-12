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
				if len(view.Sources) != 1 || view.Sources[0].Configuration.Value.Revision != 9 {
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

func TestDashboardUpdatesIgnoresOldBinaryPolicy(t *testing.T) {
	state, local, coordinator, store := dashboardGrantFixture(t)
	if _, err := store.SetConfigurationGrant(t.Context(), 0, local.ID, coordinator.ID, true, false); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"automatic.json", "automatic-consent.json"} {
		if err := os.WriteFile(filepath.Join(state, "updates", file), []byte("invalid"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := readDashboardUpdateSelection(t.Context(), state, local.ID)
	if err != nil || len(selection.Bindings) != 1 || len(selection.Problems) != 0 || selection.Bindings[0].Host.ID != coordinator.ID {
		t.Fatalf("old binary policy affected configuration status: %+v %v", selection, err)
	}
}
func TestDashboardUpdatesRejectsChangedFleetOrLocalBinding(t *testing.T) {
	state, local, coordinator, store := dashboardGrantFixture(t)
	if _, err := store.SetConfigurationGrant(t.Context(), 0, local.ID, coordinator.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if selection, err := readDashboardUpdateSelection(t.Context(), state, coordinator.ID); err != nil || len(selection.Bindings) != 0 || len(selection.Problems) != 1 {
		t.Fatalf("adopted someone else's local grant: %+v %v", selection, err)
	}
	path, _ := ConfigPath()
	saveUpdateTestFleet(t, filepath.Join(filepath.Dir(path), "fleet.json"), update.Fleet{Version: 1, Name: "changed", Revision: 2, Members: []update.Host{local}})
	if selection, err := readDashboardUpdateSelection(t.Context(), state, local.ID); err != nil || len(selection.Bindings) != 0 || len(selection.Problems) != 1 {
		t.Fatalf("searched address book for missing coordinator: %+v %v", selection, err)
	}
}
func TestDashboardUpdatesWithdrawnSelectionAndCancellationJoin(t *testing.T) {
	binding := dashboardUpdateBinding{Host: update.Host{ID: "pinned"}}
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
		*output.(*update.ConfigurationStatus) = update.ConfigurationStatus{Revision: 3}
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

	config := update.ConfigurationStatus{Change: &update.ConfigurationChange{Proposal: update.ConfigurationProposal{Recipients: []update.RecipientConfiguration{{Host: update.Host{ID: "recipient"}, Feed: &update.FeedChange{URL: wanted.URL}, Domain: &update.DomainChange{}}}}, Targets: []update.ConfigurationTarget{target}}}
	observation := DashboardStatusObservation{State: DashboardStatusObserved, At: now}
	view := DashboardUpdates{Generation: 5, Sources: []DashboardUpdateSource{{Configuration: &DashboardStatus[update.ConfigurationStatus]{observation, &config}}}}
	before, _ := json.Marshal(view)
	rows := DashboardUpdateRows(view, now)
	after, _ := json.Marshal(view)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("presentation rewrote authority or original ages")
	}
	text := fmt.Sprint(rows)
	for _, expected := range []string{"daemon restart required", "dashboard reopen required", "HTTPS verification unknown", "transport or parse failed", "Upstream observations stale", upstream.Format(time.RFC3339)} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q in %s", expected, text)
		}
	}
	rows = DashboardUpdateRows(view, now.Add(time.Minute))
	if strings.Contains(fmt.Sprint(rows), "Feed loaded") {
		t.Fatal("expired status remains current")
	}
}

func TestDashboardActualCLIInstallsPassiveUpdateAdapter(t *testing.T) {
	_, _ = setupUpdateCLI(t)
	var calls atomic.Int32
	observed := false
	caller := updateCallFunc(func(context.Context, update.Host, string, any, any) error {
		calls.Add(1)
		return errors.New("unexpected status read")
	})
	dashboard := func(ctx context.Context, input DashboardInput) error {
		if input.UpdateWatch == nil {
			return errors.New("CLI omitted update adapter")
		}
		run, cancel := context.WithCancel(ctx)
		defer cancel()
		return input.UpdateWatch(run, func(view DashboardUpdates) {
			observed = true
			if len(view.Sources) > 0 || view.Problem != "" {
				t.Errorf("unenrolled CLI input: %+v", view)
			}
			cancel()
		})
	}
	_, _, err := executeCommand(t, Dependencies{Dashboard: dashboard, UpdateCaller: caller}, "dashboard", "--wall")
	if err != nil || !observed || calls.Load() != 0 {
		t.Fatalf("actual CLI adapter: observed=%t calls=%d err=%v", observed, calls.Load(), err)
	}
}

func TestDashboardUpdatesIndependentEnrollmentFailures(t *testing.T) {
	for _, damaged := range []string{"configuration-binding", "configuration-grant", "configuration-change"} {
		t.Run(damaged, func(t *testing.T) { testDashboardIndependentEnrollment(t, damaged) })
	}
}

func testDashboardIndependentEnrollment(t *testing.T, damaged string) {
	t.Helper()
	state, local, coordinator, store := dashboardGrantFixture(t)
	path, _ := ConfigPath()
	fleet, err := update.ReadFleet(filepath.Join(filepath.Dir(path), "fleet.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecipientConsent(t.Context(), 0, update.ConsentChange{LocalHostID: local.ID, CoordinatorID: coordinator.ID, Fleet: fleet, Epoch: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetConfigurationGrant(t.Context(), 0, local.ID, coordinator.ID, true, false); err != nil {
		t.Fatal(err)
	}
	before, err := readDashboardUpdateSelection(t.Context(), state, local.ID)
	if err != nil {
		t.Fatal(err)
	}
	wanted := damageDashboardEnrollment(t, damaged, state, coordinator, store)
	calls := make(map[string]int)
	watcher := newDashboardUpdateWatcher(state, local.ID, updateCallFunc(func(_ context.Context, host update.Host, action string, _, output any) error {
		if !reflect.DeepEqual(host, coordinator) || !wanted[action] {
			t.Errorf("invalid source polled or pin substituted: %s %+v", action, host)
			return errors.New("invalid source")
		}
		calls[action]++
		if value, ok := output.(*update.ConfigurationStatus); ok {
			*value = update.ConfigurationStatus{Revision: 3}
		}
		return nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var received DashboardUpdates
	if err := watcher.Run(ctx, func(view DashboardUpdates) { received = view; cancel() }); err != nil {
		t.Fatal(err)
	}
	if received.Problem != "" {
		t.Fatalf("one enrollment failure suppressed other sources: %+v", received)
	}
	for action, expected := range wanted {
		if (calls[action] == 1) != expected {
			t.Errorf("%s calls=%d, expected enabled=%t", action, calls[action], expected)
		}
	}
	if !dashboardHasEnrollmentProblem(received) {
		t.Fatalf("invalid source disappeared rather than reporting unavailable: %+v", received)
	}
	after, err := readDashboardUpdateSelection(t.Context(), state, local.ID)
	if err != nil || after.Fingerprint == before.Fingerprint {
		t.Fatalf("source validity absent from fingerprint: %v", err)
	}
}

func damageDashboardEnrollment(t *testing.T, damaged, state string, coordinator update.Host, store *update.Store) map[string]bool {
	t.Helper()
	wanted := map[string]bool{"configuration.status": true}
	file := ""
	switch damaged {
	case "configuration-binding":
		if _, err := store.SetConfigurationGrant(t.Context(), 1, coordinator.ID, coordinator.ID, true, false); err != nil {
			t.Fatal(err)
		}
		wanted["configuration.status"] = false
	case "configuration-grant":
		file, wanted["configuration.status"] = "config-grant.json", false
	case "configuration-change":
		file = "configuration.json"
	}
	if file != "" {
		if err := os.WriteFile(filepath.Join(state, "updates", file), []byte("invalid"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return wanted
}

func dashboardHasEnrollmentProblem(view DashboardUpdates) bool {
	for _, source := range view.Sources {
		if source.Configuration != nil && source.Configuration.Observation.State == DashboardStatusUnavailable && source.Configuration.Observation.Problem != "" {
			return true
		}
	}
	return false
}

func TestDashboardUpdatesIndependentEnrollmentFencesWithdrawnResponse(t *testing.T) {
	state, local, coordinator, store := dashboardGrantFixture(t)
	path, _ := ConfigPath()
	fleet, err := update.ReadFleet(filepath.Join(filepath.Dir(path), "fleet.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecipientConsent(t.Context(), 0, update.ConsentChange{LocalHostID: local.ID, CoordinatorID: coordinator.ID, Fleet: fleet, Epoch: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetConfigurationGrant(t.Context(), 0, local.ID, coordinator.ID, true, false); err != nil {
		t.Fatal(err)
	}
	fleet.Revision++
	saveUpdateTestFleet(t, filepath.Join(filepath.Dir(path), "fleet.json"), fleet)
	calls := 0
	watcher := newDashboardUpdateWatcher(state, local.ID, updateCallFunc(func(call context.Context, host update.Host, action string, _, output any) error {
		if action != "configuration.status" || !reflect.DeepEqual(host, coordinator) {
			t.Fatalf("invalid binary enrollment queried: %s %+v", action, host)
		}
		calls++
		if _, err := store.SetConfigurationGrant(call, 1, local.ID, "", false, false); err != nil {
			t.Fatal(err)
		}
		*output.(*update.ConfigurationStatus) = update.ConfigurationStatus{Revision: 3}
		return nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := watcher.Run(ctx, func(view DashboardUpdates) {
		cancel()
		if len(view.Sources) != 0 {
			t.Errorf("withdrawn source published late status: %+v", view)
		}
	}); err != nil || calls != 1 {
		t.Fatalf("independent configuration observation calls=%d: %v", calls, err)
	}
}
