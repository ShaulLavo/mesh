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
			view.Sources = w.observe(ctx, next.Bindings)
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
func (w *dashboardUpdateWatcher) observe(ctx context.Context, bindings []dashboardUpdateBinding) []DashboardUpdateSource {
	result := make([]DashboardUpdateSource, 0, len(bindings))
	for _, binding := range bindings {
		if ctx.Err() != nil {
			break
		}
		source := DashboardUpdateSource{Coordinator: binding.Host}
		if binding.Automatic {
			var status update.AutomaticStatus
			observation, err := dashboardStatusCall(ctx, w.caller, binding.Host, "automatic.status", nil, &status)
			source.Automatic = &DashboardStatus[update.AutomaticStatus]{Observation: observation}
			if err == nil {
				source.Automatic.Value = &status
			}
		}
		if binding.Configuration && ctx.Err() == nil {
			var status update.ConfigurationStatus
			observation, err := dashboardStatusCall(ctx, w.caller, binding.Host, "configuration.status", update.Operation{}, &status)
			source.Configuration = &DashboardStatus[update.ConfigurationStatus]{Observation: observation}
			if err == nil {
				source.Configuration.Value = &status
			}
		}
		result = append(result, source)
	}
	return result
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
	automatic, err := store.ReadAutomatic(ctx)
	if err != nil {
		return selection, fmt.Errorf("read saved automatic policy: %w", err)
	}
	consent, err := store.RecipientConsent(ctx)
	if err != nil {
		return selection, fmt.Errorf("read saved binary consent: %w", err)
	}
	grant, err := store.ConfigurationGrant(ctx)
	if err != nil {
		return selection, fmt.Errorf("read saved configuration grant: %w", err)
	}
	configuration, err := store.ConfigurationStatus(ctx, "")
	if err != nil {
		return selection, fmt.Errorf("read saved configuration change: %w", err)
	}
	if err := selection.addLocal(update.LocalHost(stateDir, localID), automatic, configuration); err != nil {
		return selection, err
	}
	if consent.Enabled || grant.UsageFeed || grant.DomainPolicy {
		if err := selection.addRecipientBindings(localID, consent, grant); err != nil {
			return selection, err
		}
	}
	data, err := json.Marshal(struct {
		Policy                 *update.AutomaticPolicy
		Consent                update.RecipientConsent
		Grant                  update.ConfigurationGrant
		ChangeID, ChangeDigest string
		ConfigurationRevision  uint64
		Bindings               []dashboardUpdateBinding
	}{dashboardAutomaticPolicy(automatic), consent, grant, dashboardConfigurationIdentity(configuration), dashboardConfigurationDigest(configuration), configuration.Revision, selection.Bindings})
	if err != nil {
		return selection, fmt.Errorf("identify dashboard enrollment: %w", err)
	}
	selection.Fingerprint = sha256.Sum256(data)
	return selection, nil
}
func (s *dashboardUpdateSelection) addRecipientBindings(localID string, consent update.RecipientConsent, grant update.ConfigurationGrant) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	fleet, err := update.ReadFleet(filepath.Join(filepath.Dir(path), "fleet.json"))
	if err != nil {
		return fmt.Errorf("read pinned coordinator fleet: %w", err)
	}
	if consent.Enabled {
		if consent.LocalHostID != localID || consent.Membership != fleet.Digest() {
			return errors.New("saved binary enrollment differs from local identity or pinned fleet")
		}
		if err := s.addPinned(fleet, consent.CoordinatorID, true); err != nil {
			return err
		}
	}
	if grant.UsageFeed || grant.DomainPolicy {
		if grant.LocalHostID != localID {
			return errors.New("configuration grant belongs to another local identity")
		}
		if err := s.addPinned(fleet, grant.CoordinatorID, false); err != nil {
			return err
		}
	}
	return nil
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

func (s *dashboardUpdateSelection) addLocal(local update.Host, automatic update.AutomaticStatus, configuration update.ConfigurationStatus) error {
	if automatic.Record != nil && automatic.Record.Policy.CoordinatorID != local.ID {
		return errors.New("saved automatic policy belongs to another coordinator")
	}
	if configuration.Change != nil && configuration.Change.Proposal.CoordinatorID != local.ID {
		return errors.New("saved configuration belongs to another coordinator")
	}
	if automatic.Record != nil {
		s.add(local, true, false)
	}
	if configuration.Change != nil {
		s.add(local, false, true)
	}
	return nil
}
