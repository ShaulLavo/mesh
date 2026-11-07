package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/spf13/cobra"
)

func listAgentServices(ctx context.Context, host HostRecord, dial HostDialer) (remoteServiceSnapshot, error) {
	conn, info, err := openAgentHostInfo(ctx, host, dial)
	if err != nil {
		return remoteServiceSnapshot{}, err
	}
	defer conn.Close() //nolint:errcheck // the read result takes precedence
	id, err := newDaemonRequestID()
	if err != nil {
		return remoteServiceSnapshot{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeServiceList, RequestID: id})
	if err != nil {
		return remoteServiceSnapshot{}, err
	}
	return decodeRemoteServices(host, response, info)
}

type agentServiceCache struct {
	cache *SQLiteCatalogCache
	err   error
}

func (c *agentServiceCache) LoadAllServices(ctx context.Context) (map[string][]storage.CachedService, error) {
	var rows map[string][]storage.CachedService
	if c.err == nil {
		rows, c.err = c.cache.LoadAllServices(ctx)
	}
	if err := ctx.Err(); err != nil {
		return rows, fmt.Errorf("load passive service cache: %w", err)
	}
	return rows, nil
}

func (c *agentServiceCache) SaveServices(ctx context.Context, host HostRecord, name string, rows []protocol.ServiceInfo) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.cache.SaveServices(ctx, host, name, rows)
}

func (a *application) runAgentServices(cmd *cobra.Command, timeout time.Duration) error {
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: agentapi.ServicesData{Services: []agentapi.ServiceRow{}, HostErrors: []agentapi.Error{}}})
	}
	hosts, err = agentServiceTargets(cmd.Context(), hosts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	cache, cacheErr := openAgentCatalogCache(ctx)
	if cache == nil {
		cache = &SQLiteCatalogCache{}
	}
	defer cache.Close() //nolint:errcheck // the read result takes precedence
	adapter := &agentServiceCache{cache: cache, err: cacheErr}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return agentapi.Fail(agentapi.StateUnavailable, "services.list", "", "Service cache read exceeded the deadline.")
	}
	rows, diagnostics, err := CollectServiceCatalog(cmd.Context(), hosts, remaining, func(ctx context.Context, host HostRecord) (remoteServiceSnapshot, error) {
		return listAgentServices(ctx, host, a.dependencies.DialPassive)
	}, adapter)
	if err != nil {
		return err
	}
	if adapter.err != nil && !serviceCacheWarningReported(diagnostics) {
		diagnostics[""] = agentapi.Fail(agentapi.StateUnavailable, "services.list", "", adapter.err.Error())
	}
	return a.agent.write(cmd.OutOrStdout(), servicesEnvelope(rows, diagnostics, hosts, a.dependencies.Now().UTC()))
}

func serviceCacheWarningReported(diagnostics map[string]error) bool {
	for _, err := range diagnostics {
		var warning catalogCacheWarning
		if errors.As(err, &warning) {
			return true
		}
	}
	return false
}

func agentServiceTargets(ctx context.Context, hosts []HostRecord) ([]HostRecord, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate passive service state: %w", err)
	}
	local, err := existingLocalIdentity(state)
	if err != nil {
		return nil, err
	}
	for i, host := range hosts {
		if host.ID != local.ID {
			continue
		}
		resolved, err := resolveAgentHost(ctx, host.ID, hosts, true, nil)
		if err != nil {
			return nil, err
		}
		hosts[i] = resolved
	}
	return hosts, nil
}

func servicesEnvelope(rows []ServiceCatalogRow, diagnostics map[string]error, hosts []HostRecord, observed time.Time) agentapi.Envelope {
	data := agentapi.ServicesData{Services: make([]agentapi.ServiceRow, len(rows)), HostErrors: []agentapi.Error{}}
	for i, row := range rows {
		data.Services[i] = agentServiceRow(row, observed)
	}
	ids := make([]string, 0, len(diagnostics))
	for id := range diagnostics {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		data.HostErrors = append(data.HostErrors, serviceAgentError(diagnostics[id], id))
	}
	envelope := agentapi.Envelope{Status: agentapi.StatusOK, Data: data, Errors: data.HostErrors}
	if len(data.HostErrors) == 0 {
		return envelope
	}
	envelope.Status = agentapi.StatusPartial
	if len(rows) == 0 && !hasSuccessfulServiceHost(hosts, diagnostics) {
		envelope.Status, envelope.Data = agentapi.StatusError, nil
	}
	return envelope
}

func serviceAgentError(err error, target string) agentapi.Error {
	code := agentapi.HostUnreachable
	var warning catalogCacheWarning
	if errors.As(err, &warning) {
		code = agentapi.StateUnavailable
	}
	return agentError(err, "services.list", target, code)
}

func hasSuccessfulServiceHost(hosts []HostRecord, diagnostics map[string]error) bool {
	for _, host := range hosts {
		var warning catalogCacheWarning
		err := diagnostics[host.ID]
		if err == nil || errors.As(err, &warning) {
			return true
		}
	}
	return false
}

func agentServiceRow(row ServiceCatalogRow, observed time.Time) agentapi.ServiceRow {
	result := agentapi.ServiceRow{HostID: row.Host.ID, MachineName: row.Host.MachineName, Name: row.Service.Name, DisplayName: serviceDisplayName(row.Service), Kind: row.Service.Kind, Scope: row.Scope(), Health: row.Health(), Problem: safeRemoteText(row.Service.Problem), Freshness: agentapi.FreshnessCached}
	if row.Live {
		result.Freshness, result.ObservedAt = agentapi.FreshnessLive, &observed
	}
	if !row.Live && !row.ObservedAt.IsZero() {
		at := row.ObservedAt.UTC()
		result.ObservedAt = &at
	}
	if url := row.URL(); row.Live && url != unavailableServiceURL {
		result.URL = &url
	}
	return result
}
