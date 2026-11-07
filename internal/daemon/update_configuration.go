package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/update"
)

const (
	configurationCancelAction = "configuration.cancel"
)

type configurationObservers struct {
	mu     sync.Mutex
	latest map[string]update.ConfigurationObservation
}

func (o *configurationObservers) snapshot(now time.Time) []update.ConfigurationObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expire(now)
	result := make([]update.ConfigurationObservation, 0, len(o.latest))
	for _, observation := range o.latest {
		result = append(result, observation)
	}
	return result
}
func (o *configurationObservers) expire(now time.Time) {
	for id, observation := range o.latest {
		if now.Sub(observation.RenewedAt) >= update.ConfigurationObserverLease {
			delete(o.latest, id)
		}
	}
}
func (o *configurationObservers) accept(hostID string, observation update.ConfigurationObservation) (update.ConfigurationObservation, error) {
	if observation.HostID != hostID || !update.ValidRunID(observation.InstanceID) || observation.Generation == 0 || len(observation.Digest) != 64 || observation.State != "enabled" && observation.State != "disabled" && observation.State != "invalid" || observation.Enabled != (observation.State == "enabled") || len(observation.Problem) > 512 {
		return observation, errors.New("configuration: invalid dashboard observation")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now().UTC()
	o.expire(now)
	if o.latest == nil {
		o.latest = make(map[string]update.ConfigurationObservation)
	}
	prior, exists := o.latest[observation.InstanceID]
	if exists && observation.Generation < prior.Generation {
		return observation, errors.New("configuration: stale dashboard generation")
	}
	if exists && observation.Generation == prior.Generation && observation.Publication < prior.Publication {
		return observation, errors.New("configuration: stale dashboard publication")
	}
	if !exists && len(o.latest) >= 16 {
		return observation, errors.New("configuration: dashboard observation capacity reached")
	}
	observation.RenewedAt = now
	o.latest[observation.InstanceID] = observation
	return observation, nil
}
func (u *updateController) configuration(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (result any, err error) {
	switch action {
	case "configuration.preview":
		var selection update.ConfigurationSelection
		if err := decodeConfigurationControl(data, &selection); err != nil {
			return nil, err
		}
		result, err = u.coordinator.PreviewConfiguration(ctx, actor, selection)
	case "configuration.approve":
		var approval update.ConfigurationApproval
		if err := decodeConfigurationControl(data, &approval); err != nil {
			return nil, err
		}
		result, err = u.coordinator.ApproveConfiguration(ctx, actor, approval)
	case "configuration.retry", configurationCancelAction:
		var command update.ConfigurationCommand
		if err := decodeConfigurationControl(data, &command); err != nil {
			return nil, err
		}
		kind := strings.TrimPrefix(action, "configuration.")
		result, err = u.coordinator.ChangeConfiguration(ctx, actor, kind, command)
	case "configuration.status":
		var selector struct {
			ID string `json:"id"`
		}
		if err := decodeConfigurationControl(data, &selector); err != nil {
			return nil, err
		}
		result, err = u.coordinator.Store.ConfigurationStatus(ctx, selector.ID)
	case "configuration.snapshot":
		result, err = u.coordinator.Store.ConfigurationSnapshot(ctx, u.id, u.configurationWriter, u.configurationObservers.snapshot(time.Now()))
	case "configuration.put":
		var request update.ConfigurationPut
		if err := decodeConfigurationControl(data, &request); err != nil {
			return nil, err
		}
		result, err = u.coordinator.Store.PutConfiguration(ctx, actor, u.id, u.authority.IsAdministrator, u.configurationWriter, request)
	case "configuration.observe":
		var observation update.ConfigurationObservation
		if err := decodeConfigurationControl(data, &observation); err != nil {
			return nil, err
		}
		result, err = u.configurationObservers.accept(u.id, observation)
	default:
		return nil, fmt.Errorf("unknown configuration action %q", action)
	}
	if err != nil {
		return nil, fmt.Errorf("configuration control: %w", err)
	}
	return result, nil
}

func decodeConfigurationControl(data []byte, target any) error {
	if err := update.DecodeConfiguration(data, target); err != nil {
		return fmt.Errorf("configuration input: %w", err)
	}
	return nil
}
