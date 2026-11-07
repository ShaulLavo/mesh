package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/update"
)

const dashboardUpdateInterval = 30 * time.Second
const dashboardUpdateDeadline = 5 * time.Second

type dashboardUpdateBinding struct {
	Host                     update.Host
	Automatic, Configuration bool
}
type dashboardUpdateSelection struct {
	Bindings    []dashboardUpdateBinding
	Problems    []DashboardUpdateSource
	Fingerprint [32]byte
}
type dashboardUpdateWatcher struct {
	read     func(context.Context) (dashboardUpdateSelection, error)
	caller   update.Caller
	interval time.Duration
}

func newDashboardUpdateWatcher(stateDir, localID string, caller update.Caller) *dashboardUpdateWatcher {
	return &dashboardUpdateWatcher{read: func(ctx context.Context) (dashboardUpdateSelection, error) {
		return readDashboardUpdateSelection(ctx, stateDir, localID)
	}, caller: caller, interval: dashboardUpdateInterval}
}

func (w *dashboardUpdateWatcher) Run(ctx context.Context, publish func(DashboardUpdates)) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	var generation uint64
	for ctx.Err() == nil {
		generation++
		next, err := w.read(ctx)
		view := DashboardUpdates{Generation: generation}
		if err != nil {
			view.Problem = "Saved update status unavailable; inspect mesh update status --details."
		} else {
			view.Sources = w.observe(ctx, next)
		}
		current, currentErr := w.read(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if currentErr == nil && err == nil && current.Fingerprint != next.Fingerprint {
			publish(DashboardUpdates{Generation: generation})
			continue
		}
		if currentErr != nil && err == nil {
			view.Sources = nil
			view.Problem = "Saved update status unavailable; inspect mesh update status --details."
		}
		publish(view)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}
func (w *dashboardUpdateWatcher) observe(ctx context.Context, selection dashboardUpdateSelection) []DashboardUpdateSource {
	result := make([]DashboardUpdateSource, 0, len(selection.Problems)+len(selection.Bindings))
	result = append(result, selection.Problems...)
	for _, binding := range selection.Bindings {
		if ctx.Err() != nil {
			break
		}
		source := DashboardUpdateSource{Coordinator: binding.Host}
		if binding.Automatic {
			source.Automatic = readDashboardStatus[update.AutomaticStatus](ctx, w.caller, binding.Host, "automatic.status", nil)
		}
		if binding.Configuration && ctx.Err() == nil {
			source.Configuration = readDashboardStatus[update.ConfigurationStatus](ctx, w.caller, binding.Host, "configuration.status", update.Operation{})
		}
		result = append(result, source)
	}
	return result
}
func readDashboardStatus[T any](ctx context.Context, caller update.Caller, host update.Host, action string, input any) *DashboardStatus[T] {
	var value T
	observation, err := dashboardStatusCall(ctx, caller, host, action, input, &value)
	status := &DashboardStatus[T]{Observation: observation}
	if err == nil {
		status.Value = &value
	}
	return status
}
func dashboardStatusCall(ctx context.Context, caller update.Caller, host update.Host, action string, input, output any) (DashboardStatusObservation, error) {
	observation := DashboardStatusObservation{State: DashboardStatusUnavailable}
	if caller == nil {
		return observation, errors.New("existing update authority unavailable")
	}
	call, cancel := context.WithTimeout(ctx, dashboardUpdateDeadline)
	defer cancel()
	err := caller.Call(call, host, action, input, output)
	observation.At = time.Now().UTC()
	if err == nil {
		observation.State = DashboardStatusObserved
		return observation, nil
	}
	observation.Problem = err.Error()
	var remote *update.RemoteError
	if errors.As(err, &remote) {
		observation.State = DashboardStatusRefused
	}
	return observation, fmt.Errorf("read %s status: %w", action, err)
}

func readDashboardUpdateSelection(ctx context.Context, stateDir, localID string) (dashboardUpdateSelection, error) {
	var selection dashboardUpdateSelection
	if err := ctx.Err(); err != nil {
		return selection, fmt.Errorf("read dashboard update enrollment: %w", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "updates", "runs")); errors.Is(err, os.ErrNotExist) {
		return selection, nil
	} else if err != nil {
		return selection, fmt.Errorf("inspect saved updates: %w", err)
	}
	store, err := update.OpenStore(stateDir)
	if err != nil {
		return selection, fmt.Errorf("open saved updates: %w", err)
	}
	local := update.LocalHost(stateDir, localID)
	automatic, err := store.ReadAutomatic(ctx)
	if selection.unavailable(local, true, err) {
		automatic = update.AutomaticStatus{}
	}
	consent, err := store.RecipientConsent(ctx)
	if selection.unavailable(update.Host{}, true, err) {
		consent = update.RecipientConsent{}
	}
	grant, err := store.ConfigurationGrant(ctx)
	if selection.unavailable(update.Host{}, false, err) {
		grant = update.ConfigurationGrant{}
	}
	configuration, err := store.ConfigurationStatus(ctx, "")
	if selection.unavailable(local, false, err) {
		configuration = update.ConfigurationStatus{}
	}
	selection.addLocal(local, automatic, configuration)
	if consent.Enabled || grant.UsageFeed || grant.DomainPolicy {
		selection.addRecipientBindings(localID, consent, grant)
	}
	data, err := json.Marshal(struct {
		Policy                 *update.AutomaticPolicy
		Consent                update.RecipientConsent
		Grant                  update.ConfigurationGrant
		ChangeID, ChangeDigest string
		ConfigurationRevision  uint64
		Bindings               []dashboardUpdateBinding
		Problems               []DashboardUpdateSource
	}{dashboardAutomaticPolicy(automatic), consent, grant, dashboardConfigurationIdentity(configuration), dashboardConfigurationDigest(configuration), configuration.Revision, selection.Bindings, selection.Problems})
	if err != nil {
		return selection, fmt.Errorf("identify dashboard enrollment: %w", err)
	}
	selection.Fingerprint = sha256.Sum256(data)
	return selection, nil
}
func (s *dashboardUpdateSelection) unavailable(host update.Host, automatic bool, err error) bool {
	if err == nil {
		return false
	}
	source := DashboardUpdateSource{Coordinator: host}
	observation := DashboardStatusObservation{State: DashboardStatusUnavailable, Problem: err.Error()}
	if automatic {
		source.Automatic = &DashboardStatus[update.AutomaticStatus]{Observation: observation}
	} else {
		source.Configuration = &DashboardStatus[update.ConfigurationStatus]{Observation: observation}
	}
	s.Problems = append(s.Problems, source)
	return true
}
func (s *dashboardUpdateSelection) addRecipientBindings(localID string, consent update.RecipientConsent, grant update.ConfigurationGrant) {
	path, err := ConfigPath()
	var fleet update.Fleet
	if err == nil {
		fleet, err = update.ReadFleet(filepath.Join(filepath.Dir(path), "fleet.json"))
	}
	if consent.Enabled {
		s.unavailable(update.Host{}, true, s.addBinaryRecipient(localID, consent, fleet, err))
	}
	if grant.UsageFeed || grant.DomainPolicy {
		s.unavailable(update.Host{}, false, s.addConfigurationRecipient(localID, grant, fleet, err))
	}
}
func (s *dashboardUpdateSelection) addBinaryRecipient(localID string, consent update.RecipientConsent, fleet update.Fleet, fleetErr error) error {
	if fleetErr != nil {
		return fmt.Errorf("read pinned binary coordinator fleet: %w", fleetErr)
	}
	if consent.LocalHostID != localID || consent.Membership != fleet.Digest() {
		return errors.New("saved binary enrollment differs from local identity or pinned fleet")
	}
	return s.addPinned(fleet, consent.CoordinatorID, true)
}
func (s *dashboardUpdateSelection) addConfigurationRecipient(localID string, grant update.ConfigurationGrant, fleet update.Fleet, fleetErr error) error {
	if fleetErr != nil {
		return fmt.Errorf("read pinned configuration coordinator fleet: %w", fleetErr)
	}
	if grant.LocalHostID != localID {
		return errors.New("configuration grant belongs to another local identity")
	}
	return s.addPinned(fleet, grant.CoordinatorID, false)
}
func (s *dashboardUpdateSelection) addPinned(fleet update.Fleet, id string, automatic bool) error {
	index := slices.IndexFunc(fleet.Members, func(host update.Host) bool { return host.ID == id })
	if index < 0 {
		return errors.New("saved coordinator has no exact pinned fleet endpoint")
	}
	s.add(fleet.Members[index], automatic, !automatic)
	return nil
}
func (s *dashboardUpdateSelection) add(host update.Host, automatic, configuration bool) {
	index := slices.IndexFunc(s.Bindings, func(binding dashboardUpdateBinding) bool {
		return binding.Host.ID == host.ID && binding.Host.Endpoint == host.Endpoint
	})
	if index < 0 {
		s.Bindings = append(s.Bindings, dashboardUpdateBinding{host, automatic, configuration})
		return
	}
	s.Bindings[index].Automatic = s.Bindings[index].Automatic || automatic
	s.Bindings[index].Configuration = s.Bindings[index].Configuration || configuration
}
func existingDashboardUpdateCaller(stateDir, localID string) update.Caller {
	host, key, err := identity.LoadPrivate(stateDir)
	if err != nil || host.ID != localID {
		return nil
	}
	return update.Client{ID: host.ID, Key: key}
}

func dashboardAutomaticPolicy(status update.AutomaticStatus) *update.AutomaticPolicy {
	if status.Record == nil {
		return nil
	}
	return &status.Record.Policy
}
func dashboardConfigurationIdentity(status update.ConfigurationStatus) string {
	if status.Change == nil {
		return ""
	}
	return status.Change.Proposal.ID
}
func dashboardConfigurationDigest(status update.ConfigurationStatus) string {
	if status.Change == nil {
		return ""
	}
	return status.Change.Proposal.Digest
}

func (s *dashboardUpdateSelection) addLocal(local update.Host, automatic update.AutomaticStatus, configuration update.ConfigurationStatus) {
	if automatic.Record != nil {
		s.unavailable(local, true, s.addLocalCoordinator(local, automatic.Record.Policy.CoordinatorID, true))
	}
	if configuration.Change != nil {
		s.unavailable(local, false, s.addLocalCoordinator(local, configuration.Change.Proposal.CoordinatorID, false))
	}
}
func (s *dashboardUpdateSelection) addLocalCoordinator(local update.Host, coordinatorID string, automatic bool) error {
	if coordinatorID != local.ID {
		return errors.New("saved status belongs to another coordinator")
	}
	s.add(local, automatic, !automatic)
	return nil
}
