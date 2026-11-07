package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/storage"
)

var errCatalogPolicyChanged = errors.New("domain policy changed; reopen this command to load the saved policy")

func (c *SQLiteCatalogCache) bindServicePolicy(ctx context.Context) error {
	c.policyMu.Lock()
	defer c.policyMu.Unlock()
	token, err := domainpolicy.ConsumerCatalogToken(c.store.CatalogDirectory())
	if err != nil {
		return fmt.Errorf("bind service catalog policy: %w", err)
	}
	c.policyToken = token
	err = domainpolicy.WithCatalogGate(ctx, c.store.CatalogDirectory(), func(gate *domainpolicy.CatalogGate) error {
		if token.Digest != domainpolicy.Current().Digest() || !gate.Current(token) {
			return errCatalogPolicyChanged
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("service catalog policy: %w", err)
	}
	return nil
}
func (c *SQLiteCatalogCache) withServicePolicy(ctx context.Context, operation func(*domainpolicy.CatalogGate) error) error {
	c.policyMu.Lock()
	defer c.policyMu.Unlock()
	token, err := domainpolicy.ConsumerCatalogToken(c.store.CatalogDirectory())
	if err != nil {
		return fmt.Errorf("bind service catalog policy: %w", err)
	}
	c.policyToken = token
	err = domainpolicy.WithCatalogGate(ctx, c.store.CatalogDirectory(), func(gate *domainpolicy.CatalogGate) error {
		if err := c.settleServiceGeneration(ctx, gate); err != nil {
			return err
		}
		if token.Digest != domainpolicy.Current().Digest() || !gate.Current(token) {
			return errCatalogPolicyChanged
		}
		return operation(gate)
	})
	if err != nil {
		return fmt.Errorf("service catalog policy: %w", err)
	}
	return nil
}
func advisoryServicePolicyError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if !errors.Is(err, errCatalogPolicyChanged) && !errors.Is(err, storage.ErrAdvisoryWriteBusy) && !errors.Is(err, domainpolicy.ErrPolicyLockBusy) && !errors.Is(err, domainpolicy.ErrPublicationPending) {
		return false
	}
	log.Printf("catalog policy: skipped advisory access: %v", err)
	return true
}

func loadedServicePolicy(ctx context.Context) (domainpolicy.CatalogToken, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return domainpolicy.CatalogToken{}, fmt.Errorf("locate service policy: %w", err)
	}
	token, err := domainpolicy.ConsumerCatalogToken(state)
	if err != nil {
		return token, fmt.Errorf("load process service policy: %w", err)
	}
	_, err = observeServicePolicy(ctx, token)
	return token, err
}

type servicePolicyObservation uint8

const (
	servicePolicyUnavailable servicePolicyObservation = iota
	servicePolicyKnownCurrent
	servicePolicyChanged
)

func observeServicePolicy(ctx context.Context, token domainpolicy.CatalogToken) (servicePolicyObservation, error) {
	if token.Generation == 0 {
		return servicePolicyUnavailable, errors.New("service policy generation is unknown")
	}
	if token.Digest != domainpolicy.Current().Digest() {
		return servicePolicyChanged, errCatalogPolicyChanged
	}
	if err := ctx.Err(); err != nil {
		return servicePolicyUnavailable, fmt.Errorf("observe service policy: %w", err)
	}
	state, err := paths.StateDirPath()
	if err != nil {
		return servicePolicyUnavailable, fmt.Errorf("locate service policy: %w", err)
	}
	pinned, err := domainpolicy.ConsumerCatalogToken(state)
	if err != nil {
		return servicePolicyUnavailable, fmt.Errorf("process service policy unavailable: %w", err)
	}
	if pinned != token {
		return servicePolicyChanged, errCatalogPolicyChanged
	}
	policy, err := domainpolicy.ReadCatalogPolicy(state)
	if err != nil {
		return servicePolicyUnavailable, fmt.Errorf("service policy temporarily unavailable: %w", err)
	}
	if policy.Pending || policy.CatalogToken != token {
		return servicePolicyChanged, errCatalogPolicyChanged
	}
	if _, err := initialServicePolicy(); err != nil {
		if errors.Is(err, errCatalogPolicyChanged) {
			return servicePolicyChanged, err
		}
		return servicePolicyUnavailable, err
	}
	return servicePolicyKnownCurrent, nil
}
func servicePolicyCurrent(ctx context.Context, token domainpolicy.CatalogToken) bool {
	status, _ := observeServicePolicy(ctx, token)
	return status == servicePolicyKnownCurrent
}
func fenceServiceState(ctx context.Context, view StateView) StateView {
	status, err := observeServicePolicy(ctx, view.Policy)
	if status == servicePolicyKnownCurrent {
		return view
	}
	view = view.Clone()
	if status == servicePolicyChanged || view.Policy.Generation == 0 {
		view = invalidateServiceState(view)
		view.Problem = err.Error()
		return view
	}
	markSectionFailed(&view, "services")
	markSectionFailed(&view, "host")
	view.NameVerified, view.Problem = false, err.Error()
	return view
}
func invalidateServiceState(view StateView) StateView {
	view.Services, view.PrivateName, view.NameVerified = nil, "", false
	markSectionFailed(&view, "services")
	markSectionFailed(&view, "host")
	view.Problem = fmt.Sprint(errCatalogPolicyChanged)
	return view
}

func settleSavedCatalogPolicy(digest string) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := domainpolicy.SettleSavedPolicy(filepath.Join(filepath.Dir(path), "domains.json"), digest); err != nil {
		return fmt.Errorf("settle saved catalog policy: %w", err)
	}
	return nil
}

func (c *SQLiteCatalogCache) settleServiceGeneration(ctx context.Context, gate *domainpolicy.CatalogGate) error {
	err := gate.Settle(func(ids []string) error {
		if err := settleSavedCatalogPolicy(gate.Policy.Digest); err != nil {
			return err
		}
		if err := c.store.InvalidateCachedServices(ctx, ids); err != nil {
			return fmt.Errorf("invalidate service generation: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("settle service generation: %w", err)
	}
	return nil
}

// Passive commands observe the same initial generation the first writer publishes,
// without creating an optional state directory, lock or database.
func initialServicePolicy() (domainpolicy.CatalogToken, error) {
	path, err := ConfigPath()
	if err != nil {
		return domainpolicy.CatalogToken{}, err
	}
	policy, present, err := domainpolicy.ReadSavedPolicy(filepath.Join(filepath.Dir(path), "domains.json"))
	if err != nil {
		return domainpolicy.CatalogToken{}, fmt.Errorf("read initial service policy: %w", err)
	}
	loaded := domainpolicy.Current().Digest()
	if present && policy.Digest() != loaded {
		return domainpolicy.CatalogToken{}, errCatalogPolicyChanged
	}
	return domainpolicy.CatalogToken{Digest: loaded, Generation: domainpolicy.InitialCatalogGeneration}, nil
}
