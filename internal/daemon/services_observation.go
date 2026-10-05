package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

const serviceHealthReadTimeout = 1500 * time.Millisecond

// The joined reconciliation loop owns periodic observations; watch setup shares
// its admission and recent result, without starting another probing loop.
func (c *serviceController) observeRegistry(ctx context.Context, observe func(error)) {
	readCtx, cancel := context.WithTimeout(ctx, serviceHealthReadTimeout)
	defer cancel()
	err := c.observeHealth(readCtx)
	observe(err)
}

func (c *serviceController) observeHealth(ctx context.Context) error {
	select {
	case c.healthGate <- struct{}{}:
		defer func() { <-c.healthGate }()
	case <-ctx.Done():
		return fmt.Errorf("service health admission: %w", ctx.Err())
	}
	registered, err := c.healthCatalog(ctx)
	if err != nil {
		return err
	}
	if registered == nil {
		return nil
	}
	// serviceStatuses probes loopback directly and skips non-running demand;
	// it never invokes an HTTP handler, a wake, or a demand start.
	rows := make(map[string]protocol.ServiceInfo, len(registered))
	for _, row := range c.serviceStatuses(ctx, registered) {
		rows[row.Name] = row
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("service health observation: %w", err)
	}
	return c.commitObservedHealth(ctx, rows)
}

func (c *serviceController) commitObservedHealth(ctx context.Context, rows map[string]protocol.ServiceInfo) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-c.gate }()
	if c.catalogUnknown || !c.healthMatches(rows, c.registry.Services()) {
		c.publishCommitted()
		return errors.New("service health observation changed before publication")
	}
	c.observedHealth, c.healthAt = rows, time.Now()
	c.publishCommitted()
	return nil
}

func (c *serviceController) healthCatalog(ctx context.Context) ([]meshserve.Service, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.gate }()
	if c.catalogUnknown {
		return nil, errors.New("service catalog is unavailable")
	}
	registered := c.registry.Services()
	if time.Since(c.healthAt) < defaultReconcileInterval && c.healthMatches(c.observedHealth, registered) {
		return nil, nil
	}
	return registered, nil
}

func (c *serviceController) healthMatches(rows map[string]protocol.ServiceInfo, registered []meshserve.Service) bool {
	if len(rows) != len(registered) {
		return false
	}
	for _, service := range registered {
		row, found := rows[service.Name]
		if !found || !service.Equal(serviceFromInfo(row)) || !sameDemandHealth(row.Demand, c.demand.Status(service.Name)) {
			return false
		}
	}
	return true
}

func (c *serviceController) publishedServiceHealth(service meshserve.Service) protocol.ServiceInfo {
	row := serviceDefinitionInfo(service)
	row.Demand, row.HealthUnknown = c.demand.Status(service.Name), true
	if time.Since(c.healthAt) >= 30*time.Second {
		return row
	}
	observed, found := c.observedHealth[service.Name]
	if found && service.Equal(serviceFromInfo(observed)) && sameDemandHealth(row.Demand, observed.Demand) {
		row.Healthy, row.Problem, row.HealthUnknown = observed.Healthy, observed.Problem, false
	}
	return row
}

func sameDemandHealth(a, b *protocol.ServiceDemand) bool {
	if a == nil || b == nil {
		return a == b
	}
	left, right := *a, *b
	// Connection counts change during traffic without changing the probe's service identity.
	left.Connections, right.Connections = 0, 0
	return reflect.DeepEqual(left, right)
}
