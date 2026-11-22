package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
)

// hostedCatalog reads the hosted MCP catalog of hosts passively, scoped to
// this frontend's caller, with the private catalog cache as fallback.
func (c *mcpControls) hostedCatalog(ctx context.Context, hosts []HostRecord, timeout time.Duration) []agentapi.MCPCatalogService {
	state, err := paths.StateDirPath()
	if err != nil {
		return nil
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scope := mcpCallerScopeFor(c.caller, local.ID)
	results := collectMCPCatalogHosts(ctx, hosts, c.pool, openMCPCatalogCache(state), c.caller, scope, false, c.app.dependencies.Now().UTC())
	services := []agentapi.MCPCatalogService{}
	for _, r := range results {
		services = append(services, r.services...)
	}
	return services
}

// withHostedServices adds the hosted MCP servers of the listed hosts. A
// host that cannot host MCP servers contributes none; serve route errors
// are already reported.
func (c *mcpControls) withHostedServices(ctx context.Context, hosts []HostRecord, envelope agentapi.Envelope, timeout time.Duration) agentapi.Envelope {
	data, ok := envelope.Data.(agentapi.ServicesData)
	if !ok || len(hosts) == 0 {
		return envelope
	}
	envelope.Data = mcpnative.ServicesWithHosted{ServicesData: data, HostedMCP: c.hostedCatalog(ctx, hosts, timeout)}
	return envelope
}

// inspectHosted finds a hosted MCP server named like the requested service.
func (c *mcpControls) inspectHosted(ctx context.Context, host HostRecord, name string, timeout time.Duration, target *mcpnative.TargetMeta) (mcpnative.CallOutcome, bool) {
	for _, s := range c.hostedCatalog(ctx, []HostRecord{host}, timeout) {
		if s.HostID == host.ID && (s.Name == name || s.ID == name) {
			return mcpnative.CallOutcome{
				// The tool's own operation fills the empty name.
				Envelope: agentapi.Envelope{Status: agentapi.StatusOK, Data: mcpnative.HostedServiceDetail{HostedMCP: s}},
				Target:   target,
			}, true
		}
	}
	return mcpnative.CallOutcome{}, false
}

// LeaseService holds or releases one hosted instance through mcp.service.lease.
func (c *mcpControls) LeaseService(ctx context.Context, req mcpnative.LeaseServiceRequest) (mcpnative.CallOutcome, error) {
	const operation = "mcp.lease"
	hosts, err := LoadHosts()
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveMCPHost(ctx, req.Host, hosts, false)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	target := c.targetMeta(host, protocol.HostInfo{})
	instance, err := resolveMCPInstanceArg(operation, req.Instance, host.ID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	resp, _, err := c.pool.ExchangeMCP(ctx, host, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceLease, Service: req.Service, Instance: instance,
		LeaseID: req.LeaseID, LeaseTTLMillis: req.TTL.Milliseconds(), Job: req.Job, Caller: c.caller}, false)
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, req.Service, agentapi.HostUnreachable)), nil
	}
	if resp.Type == protocol.TypeError {
		return failOutcome(operation, target, agentapi.Fail(controlAgentCode(resp.ErrorCode), operation, req.Service, resp.Message).Err), nil
	}
	data, err := decodeLease(host.ID, req, resp.Result)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	return mcpnative.CallOutcome{Envelope: agentapi.Envelope{Operation: operation, Status: agentapi.StatusOK, Data: data}, Target: target}, nil
}

func decodeLease(hostID string, req mcpnative.LeaseServiceRequest, raw json.RawMessage) (mcpnative.ServiceLeaseData, error) {
	var payload struct {
		Lease struct {
			ID      string    `json:"id"`
			Expires time.Time `json:"expires"`
			Job     string    `json:"job"`
		} `json:"lease"`
		Instance mcphost.InstanceView `json:"instance"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return mcpnative.ServiceLeaseData{}, fmt.Errorf("decode mcp.service.lease reply: %w", err)
	}
	inst := payload.Instance
	return mcpnative.ServiceLeaseData{
		HostID: hostID, Service: req.Service, LeaseID: payload.Lease.ID, Expires: payload.Lease.Expires, Job: payload.Lease.Job, Released: req.TTL == 0,
		Instance: agentapi.MCPCatalogInstance{ID: inst.ID, Shared: inst.Shared, Mine: inst.Mine, Default: inst.Default, State: inst.State,
			Revision: inst.Revision, Generation: inst.Generation, Active: inst.Active, Leases: inst.Leases, UpdatedAt: inst.UpdatedAt},
	}, nil
}
