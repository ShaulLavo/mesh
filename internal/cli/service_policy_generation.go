package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/storage"
)

var errCatalogPolicyChanged = errors.New("domain policy changed; reopen this command to load the saved policy")

func (c *SQLiteCatalogCache) bindServicePolicy(ctx context.Context) error {
	c.policyMu.Lock()
	defer c.policyMu.Unlock()
	if c.policyToken.Generation != 0 {
		return nil
	}
	err := domainpolicy.WithCatalogGate(ctx, c.store.CatalogDirectory(), func(gate *domainpolicy.CatalogGate) error {
		if gate.Policy.Digest != domainpolicy.Current().Digest() {
			return errCatalogPolicyChanged
		}
		c.policyToken = gate.Policy.CatalogToken
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
	err := domainpolicy.WithCatalogGate(ctx, c.store.CatalogDirectory(), func(gate *domainpolicy.CatalogGate) error {
		if err := c.settleServiceGeneration(ctx, gate); err != nil {
			return err
		}
		if c.policyToken.Generation == 0 && gate.Policy.Digest == domainpolicy.Current().Digest() {
			c.policyToken = gate.Policy.CatalogToken
		}
		if !gate.Current(c.policyToken) {
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
	var token domainpolicy.CatalogToken
	if _, err := os.Lstat(filepath.Join(state, "catalog-policy.json")); errors.Is(err, os.ErrNotExist) {
		return initialServicePolicy()
	} else if err != nil {
		return token, fmt.Errorf("inspect existing catalog generation: %w", err)
	}
	err = domainpolicy.WithCatalogGate(ctx, state, func(gate *domainpolicy.CatalogGate) error {
		if err := settleCatalogGeneration(ctx, gate, state); err != nil {
			return err
		}
		if gate.Policy.Digest != domainpolicy.Current().Digest() || gate.Policy.Pending {
			return errCatalogPolicyChanged
		}
		token = gate.Policy.CatalogToken
		return nil
	})
	if err != nil {
		return token, fmt.Errorf("read loaded service policy: %w", err)
	}
	return token, nil
}
func servicePolicyCurrent(ctx context.Context, token domainpolicy.CatalogToken) bool {
	if token.Generation == 0 {
		return false
	}
	current, err := loadedServicePolicy(ctx)
	return err == nil && current == token
}
func invalidateServiceState(view StateView) StateView {
	view.Services, view.PrivateName, view.NameVerified = nil, "", false
	markSectionFailed(&view, "services")
	markSectionFailed(&view, "host")
	view.Problem = fmt.Sprint(errCatalogPolicyChanged)
	return view
}

func invalidateCatalogPolicy(ctx context.Context, directory string, ids []string) error {
	path := filepath.Join(directory, catalogDatabaseName)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect advisory catalog: %w", err)
	}
	store, err := storage.OpenAdvisory(ctx, path)
	if err != nil {
		return fmt.Errorf("open invalidated service cache: %w", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.InvalidateCachedServices(ctx, ids); err != nil {
		return fmt.Errorf("invalidate service snapshots: %w", err)
	}
	return nil
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
func settleCatalogGeneration(ctx context.Context, gate *domainpolicy.CatalogGate, directory string) error {
	err := gate.Settle(func(ids []string) error {
		if err := settleSavedCatalogPolicy(gate.Policy.Digest); err != nil {
			return err
		}
		return invalidateCatalogPolicy(ctx, directory, ids)
	})
	if err != nil {
		return fmt.Errorf("settle catalog generation: %w", err)
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
