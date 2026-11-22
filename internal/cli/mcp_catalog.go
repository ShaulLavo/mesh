package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/spf13/cobra"
)

const (
	defaultMCPListTimeout     = 2 * time.Second
	minMCPListTimeout         = 100 * time.Millisecond
	maxMCPListTimeout         = 30 * time.Second
	mcpFreshnessNone          = "none"
	mcpServiceDisabled        = "disabled"
	mcpInvalidateTimeout      = 1500 * time.Millisecond
	mcpUninitializedPrincipal = "uninitialized"
)

type mcpListOptions struct {
	host         string
	identityName string
	timeout      time.Duration
	cached       bool
	asJSON       bool
}

func (a *application) mcpListCommand() *cobra.Command {
	var o mcpListOptions
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{agentSessionListCommand},
		Short:   "List hosted MCP servers across saved Mesh hosts",
		Long: "Query saved Mesh hosts concurrently under a 2-second budget and list their\n" +
			"hosted MCP servers without waking offline hosts or starting stopped services.\n" +
			"Unreachable hosts retain eligible private cached entries for up to 24 hours\n" +
			"(marked stale, then expired); authorization denials and pin changes discard\n" +
			"cached entries immediately.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.timeout < minMCPListTimeout || o.timeout > maxMCPListTimeout {
				return agentapi.Usage("mcp.list", fmt.Sprintf("Use --timeout between %s and %s.", minMCPListTimeout, maxMCPListTimeout))
			}
			return a.runMCPList(cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.host, "host", "", "limit listing to one machine name, exact host ID, or local")
	f.StringVar(&o.identityName, "identity", "", "scope visible instances and private catalog cache to this application identity")
	f.DurationVar(&o.timeout, "timeout", defaultMCPListTimeout, "hard deadline for fleet queries (100ms to 30s)")
	f.BoolVar(&o.cached, "cached", false, "read private cached catalog entries without dialing hosts")
	f.BoolVar(&o.asJSON, "json", false, "write one agent result")
	return cmd
}

type mcpCatalogHostResult struct {
	host     agentapi.MCPCatalogHost
	services []agentapi.MCPCatalogService
	diags    []agentapi.Error
	usable   bool
}

func (a *application) runMCPList(cmd *cobra.Command, o mcpListOptions) error {
	// Honor one single deadline across host resolution, pin pruning, and fleet queries.
	queryCtx, cancel := context.WithTimeout(cmd.Context(), o.timeout)
	defer cancel()

	stateDir, err := paths.StateDirPath()
	if err != nil {
		return fmt.Errorf("locate Mesh state directory: %w", err)
	}
	local, err := agentLocalHost(stateDir)
	if err != nil {
		return err
	}
	caller, callerScope, err := resolveMCPListCaller(o.identityName, local.ID)
	if err != nil {
		return err
	}
	savedHosts, err := LoadHosts()
	if err != nil {
		return err
	}
	hosts, err := selectMCPCatalogHosts(queryCtx, o, savedHosts, local, a.dependencies.DialPassive)
	if err != nil {
		return err
	}

	cache := openMCPCatalogCache(stateDir)
	var extraDiags []agentapi.Error
	if pruneErr := pruneMCPCatalogPins(queryCtx, cache, savedHosts, local); pruneErr != nil {
		extraDiags = append(extraDiags, agentapi.Fail(agentapi.StateUnavailable, "mcp.list", "", fmt.Sprintf("prune MCP catalog cache pins: %v", pruneErr)).Err)
	}

	pool := newMCPHostPool(a.dependencies.DialPassive, a.dependencies.Now)
	defer pool.Close()

	now := a.dependencies.Now().UTC()
	results := collectMCPCatalogHosts(queryCtx, hosts, pool, cache, caller, callerScope, o.cached, now)
	data, diags, usableCount := buildMCPCatalogData(now, results, extraDiags)
	if o.asJSON {
		return a.writeMCPCatalogEnvelope(cmd.OutOrStdout(), data, diags, usableCount, len(hosts))
	}
	if err := writeMCPFleetTable(cmd.OutOrStdout(), data); err != nil {
		return err
	}
	return mcpCatalogExitError(diags, usableCount, len(hosts))
}

func resolveMCPListCaller(identityName, localHostID string) (protocol.CallerCredential, string, error) {
	principal := localHostID
	if principal == "" {
		principal = mcpUninitializedPrincipal
	}
	if strings.TrimSpace(identityName) == "" {
		return "", "device:" + principal + ":admin", nil
	}
	id, err := loadApplicationIdentity("mcp.list", identityName)
	if err != nil {
		return "", "", err
	}
	derived, err := appcaller.Derive(appcaller.Scope{Class: appcaller.ScopeDevice, Principal: principal}, id.Credential)
	if err != nil {
		return "", "", fmt.Errorf("derive MCP catalog cache caller scope: %w", err)
	}
	return protocol.CallerCredential(id.Credential.Encode()), "app:" + string(derived), nil
}

func pruneMCPCatalogPins(ctx context.Context, cache *mcpCatalogCache, saved []HostRecord, local HostRecord) error {
	pins := make(map[string]string, len(saved)+1)
	for _, h := range saved {
		if h.ID != "" {
			pins[h.ID] = h.MeshIdentity
		}
	}
	if local.ID != "" {
		pins[local.ID] = local.MeshIdentity
	}
	pruneCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpInvalidateTimeout)
	defer cancel()
	return cache.PruneMismatchedPins(pruneCtx, pins)
}

func (a *application) writeMCPCatalogEnvelope(out io.Writer, data agentapi.MCPCatalogData, diags []agentapi.Error, usableCount, hostCount int) error {
	envelope := agentapi.Envelope{Status: agentapi.StatusOK, Data: data, Errors: diags}
	if len(diags) > 0 {
		envelope.Status = agentapi.StatusPartial
		if usableCount == 0 && hostCount > 0 {
			envelope.Status = agentapi.StatusError
		}
	}
	return a.agent.write(out, envelope)
}

func mcpCatalogExitError(diags []agentapi.Error, usableCount, hostCount int) error {
	if len(diags) == 0 {
		return nil
	}
	if usableCount == 0 && hostCount > 0 {
		return statusError{code: 1}
	}
	return statusError{code: 3}
}

func selectMCPCatalogHosts(ctx context.Context, o mcpListOptions, saved []HostRecord, local HostRecord, dial HostDialer) ([]HostRecord, error) {
	if strings.TrimSpace(o.host) != "" {
		host, err := resolveMCPTargetHostWithSaved(ctx, o.host, o.cached, saved, local, dial)
		if err != nil {
			return nil, err
		}
		return []HostRecord{host}, nil
	}
	hosts := append([]HostRecord(nil), saved...)
	if local.ID != "" {
		hosts = agentHostsWithLocal(hosts, local)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	return hosts, nil
}

func collectMCPCatalogHosts(
	ctx context.Context,
	hosts []HostRecord,
	pool *mcpHostPool,
	cache *mcpCatalogCache,
	caller protocol.CallerCredential,
	callerScope string,
	cachedOnly bool,
	now time.Time,
) []mcpCatalogHostResult {
	type indexed struct {
		index  int
		result mcpCatalogHostResult
	}
	ch := make(chan indexed, len(hosts))
	for i, host := range hosts {
		go func(idx int, h HostRecord) {
			ch <- indexed{index: idx, result: queryMCPCatalogHost(ctx, h, pool, cache, caller, callerScope, cachedOnly, now)}
		}(i, host)
	}
	out := make([]mcpCatalogHostResult, len(hosts))
	done := make([]bool, len(hosts))
	received := 0
	for received < len(hosts) {
		select {
		case item := <-ch:
			out[item.index] = item.result
			done[item.index] = true
			received++
		case <-ctx.Done():
			// Deadline expired: preserve every completed host and fall back immediately
			// to cached/offline status for any still-in-flight host without waiting.
			for i, host := range hosts {
				if !done[i] {
					out[i] = catalogFromCache(host, cache, callerScope, now, ctx.Err())
				}
			}
			return out
		}
	}
	return out
}

func queryMCPCatalogHost(
	ctx context.Context,
	host HostRecord,
	pool *mcpHostPool,
	cache *mcpCatalogCache,
	caller protocol.CallerCredential,
	callerScope string,
	cachedOnly bool,
	now time.Time,
) mcpCatalogHostResult {
	if cachedOnly {
		return catalogFromCache(host, cache, callerScope, now, nil)
	}
	obsGen, obsErr := cache.BeginObservation(ctx, host.ID)
	resp, info, err := pool.ExchangeMCP(ctx, host, protocol.MCPServiceRequest{
		Type:   protocol.TypeMCPCatalog,
		Caller: caller,
	}, true)
	if err != nil {
		return handleMCPCatalogError(ctx, host, cache, caller, callerScope, now, err)
	}
	if resp.Type == protocol.TypeError {
		return handleMCPCatalogControlError(ctx, host, cache, caller, callerScope, resp)
	}
	var payload struct {
		HostID   string                   `json:"hostId"`
		Services []mcphost.CatalogService `json:"services"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		return failedCatalogHost(host, agentapi.ServiceFailed, fmt.Errorf("decode mcp.catalog reply: %w", err))
	}
	name := hostDisplayName(host, info)
	services := convertMCPServices(host, name, payload.Services, string(agentapi.FreshnessLive), &now, false)
	var diags []agentapi.Error
	if obsErr != nil {
		diags = append(diags, agentapi.Fail(agentapi.StateUnavailable, "mcp.list", host.ID, obsErr.Error()).Err)
	} else if host.ID != "" && host.MeshIdentity != "" && ctx.Err() == nil {
		if _, saveErr := cache.SaveIfCurrent(ctx, mcpCatalogCacheEntry{
			HostID:       host.ID,
			MeshIdentity: host.MeshIdentity,
			MachineName:  name,
			CallerScope:  callerScope,
			ReceivedAt:   now,
			Services:     payload.Services,
		}, obsGen); saveErr != nil {
			diags = append(diags, agentapi.Fail(agentapi.StateUnavailable, "mcp.list", host.ID, saveErr.Error()).Err)
		}
	}
	return mcpCatalogHostResult{
		host: agentapi.MCPCatalogHost{
			HostID:       host.ID,
			MachineName:  name,
			Local:        host.local,
			State:        agentapi.MCPHostOnline,
			Freshness:    string(agentapi.FreshnessLive),
			ObservedAt:   &now,
			ServiceCount: len(services),
		},
		services: services,
		diags:    diags,
		usable:   true,
	}
}

func invalidateHostCache(ctx context.Context, cache *mcpCatalogCache, hostID string) *agentapi.Error {
	invalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpInvalidateTimeout)
	defer cancel()
	if err := cache.InvalidateHost(invalCtx, hostID); err != nil {
		d := agentapi.Fail(agentapi.StateUnavailable, "mcp.list", hostID, fmt.Sprintf("invalidate host catalog cache: %v", err)).Err
		return &d
	}
	return nil
}

func invalidateCallerCache(ctx context.Context, cache *mcpCatalogCache, hostID, callerScope string) *agentapi.Error {
	invalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpInvalidateTimeout)
	defer cancel()
	if err := cache.InvalidateHostCaller(invalCtx, hostID, callerScope); err != nil {
		d := agentapi.Fail(agentapi.StateUnavailable, "mcp.list", hostID, fmt.Sprintf("invalidate caller catalog cache: %v", err)).Err
		return &d
	}
	return nil
}

func handleMCPCatalogError(ctx context.Context, host HostRecord, cache *mcpCatalogCache, caller protocol.CallerCredential, callerScope string, now time.Time, err error) mcpCatalogHostResult {
	var failure *agentapi.Failure
	var mismatch *hostIdentityError
	var rejected *ControlError
	switch {
	case errors.As(err, &mismatch), errors.Is(err, transport.ErrDestinationIdentityMismatch):
		invalErr := invalidateHostCache(ctx, cache, host.ID)
		return failedCatalogHostWithState(host, agentapi.MCPHostIdentityChanged, agentapi.HostIdentityMismatch, err.Error(), invalErr)
	case transport.IsAuthenticationInterrupted(err):
		// Interrupted handshake is a link/network failure, not an explicit grant denial.
		return catalogFromCache(host, cache, callerScope, now, err)
	case transport.IsAuthenticationRefused(err):
		// Explicit device-grant refusal invalidates EVERY caller scope on this device for this host.
		invalErr := invalidateHostCache(ctx, cache, host.ID)
		return failedCatalogHostWithState(host, agentapi.MCPHostUnauthorized, agentapi.AccessDenied, err.Error(), invalErr)
	case errors.As(err, &failure):
		return classifyAgentFailureCatalogHost(ctx, host, cache, caller, callerScope, now, failure)
	case errors.As(err, &rejected):
		return classifyControlCodeCatalogHost(ctx, host, cache, caller, callerScope, rejected.Code, rejected.Error())
	default:
		return catalogFromCache(host, cache, callerScope, now, err)
	}
}

func classifyAgentFailureCatalogHost(ctx context.Context, host HostRecord, cache *mcpCatalogCache, caller protocol.CallerCredential, callerScope string, now time.Time, failure *agentapi.Failure) mcpCatalogHostResult {
	code := failure.Err.Code
	if code == agentapi.ProtocolUnsupported {
		invalErr := invalidateCallerCache(ctx, cache, host.ID, callerScope)
		return failedCatalogHostWithState(host, agentapi.MCPHostUnsupported, agentapi.ProtocolUnsupported, failure.Err.Message, invalErr)
	}
	if code == agentapi.HostAuthenticationFailed {
		invalErr := invalidateHostCache(ctx, cache, host.ID)
		return failedCatalogHostWithState(host, agentapi.MCPHostUnauthorized, agentapi.AccessDenied, failure.Err.Message, invalErr)
	}
	if code == agentapi.AccessDenied {
		invalErr := invalidateAccessDenied(ctx, cache, host.ID, caller, callerScope)
		return failedCatalogHostWithState(host, agentapi.MCPHostUnauthorized, agentapi.AccessDenied, failure.Err.Message, invalErr)
	}
	if code == agentapi.HostIdentityMismatch {
		invalErr := invalidateHostCache(ctx, cache, host.ID)
		return failedCatalogHostWithState(host, agentapi.MCPHostIdentityChanged, agentapi.HostIdentityMismatch, failure.Err.Message, invalErr)
	}
	if code == agentapi.HostUninitialized {
		return failedCatalogHostWithState(host, agentapi.MCPHostOffline, agentapi.HostUninitialized, failure.Err.Message, nil)
	}
	return catalogFromCache(host, cache, callerScope, now, failure)
}

func handleMCPCatalogControlError(ctx context.Context, host HostRecord, cache *mcpCatalogCache, caller protocol.CallerCredential, callerScope string, resp protocol.MCPServiceResponse) mcpCatalogHostResult {
	return classifyControlCodeCatalogHost(ctx, host, cache, caller, callerScope, resp.ErrorCode, resp.Message)
}

func classifyControlCodeCatalogHost(ctx context.Context, host HostRecord, cache *mcpCatalogCache, caller protocol.CallerCredential, callerScope, rawCode, message string) mcpCatalogHostResult {
	code := controlAgentCode(rawCode)
	if code == agentapi.ProtocolUnsupported {
		invalErr := invalidateCallerCache(ctx, cache, host.ID, callerScope)
		return failedCatalogHostWithState(host, agentapi.MCPHostUnsupported, agentapi.ProtocolUnsupported, message, invalErr)
	}
	if code == agentapi.HostAuthenticationFailed {
		invalErr := invalidateHostCache(ctx, cache, host.ID)
		return failedCatalogHostWithState(host, agentapi.MCPHostUnauthorized, agentapi.AccessDenied, message, invalErr)
	}
	if code == agentapi.AccessDenied {
		invalErr := invalidateAccessDenied(ctx, cache, host.ID, caller, callerScope)
		return failedCatalogHostWithState(host, agentapi.MCPHostUnauthorized, agentapi.AccessDenied, message, invalErr)
	}
	if code == agentapi.HostIdentityMismatch {
		invalErr := invalidateHostCache(ctx, cache, host.ID)
		return failedCatalogHostWithState(host, agentapi.MCPHostIdentityChanged, agentapi.HostIdentityMismatch, message, invalErr)
	}
	return failedCatalogHostWithState(host, agentapi.MCPHostFailed, code, message, nil)
}

func invalidateAccessDenied(ctx context.Context, cache *mcpCatalogCache, hostID string, caller protocol.CallerCredential, callerScope string) *agentapi.Error {
	if caller == "" {
		return invalidateHostCache(ctx, cache, hostID)
	}
	return invalidateCallerCache(ctx, cache, hostID, callerScope)
}

func failedCatalogHost(host HostRecord, code agentapi.Code, err error) mcpCatalogHostResult {
	return failedCatalogHostWithState(host, agentapi.MCPHostFailed, code, err.Error(), nil)
}

func failedCatalogHostWithState(host HostRecord, state string, code agentapi.Code, message string, extra *agentapi.Error) mcpCatalogHostResult {
	diag := agentapi.Fail(code, "mcp.list", host.ID, message).Err
	diags := []agentapi.Error{diag}
	if extra != nil {
		diags = append(diags, *extra)
	}
	return mcpCatalogHostResult{
		host: agentapi.MCPCatalogHost{
			HostID:      host.ID,
			MachineName: host.MachineName,
			Local:       host.local,
			State:       state,
			Freshness:   mcpFreshnessNone,
			Code:        diag.Code,
			Message:     diag.Message,
		},
		diags: diags,
	}
}

func catalogFromCache(host HostRecord, cache *mcpCatalogCache, callerScope string, now time.Time, cause error) mcpCatalogHostResult {
	entry, cacheErr := cache.Read(host.ID, host.MeshIdentity, callerScope)
	var extraDiag *agentapi.Error
	if cacheErr != nil {
		d := agentapi.Fail(agentapi.StateUnavailable, "mcp.list", host.ID, cacheErr.Error()).Err
		extraDiag = &d
		entry = nil
	}
	causeMsg := "host is unreachable"
	if cause != nil {
		causeMsg = agentapi.CleanMessage(cause.Error())
	} else if entry == nil {
		causeMsg = "no cached catalog entry for this host"
	}
	if entry == nil {
		diag := agentapi.Fail(agentapi.HostUnreachable, "mcp.list", host.ID, causeMsg).Err
		diags := []agentapi.Error{diag}
		if extraDiag != nil {
			diags = append(diags, *extraDiag)
		}
		return mcpCatalogHostResult{
			host: agentapi.MCPCatalogHost{
				HostID:      host.ID,
				MachineName: host.MachineName,
				Local:       host.local,
				State:       agentapi.MCPHostOffline,
				Freshness:   mcpFreshnessNone,
				Code:        diag.Code,
				Message:     diag.Message,
			},
			diags: diags,
		}
	}
	observed := entry.ReceivedAt.UTC()
	name := host.MachineName
	if name == "" {
		name = entry.MachineName
	}
	if now.Sub(observed) > mcpCatalogCacheStaleLimit {
		msg := fmt.Sprintf("cached catalog from %s expired after 24h while host is unreachable", observed.Format(time.RFC3339))
		diag := agentapi.Fail(agentapi.ResultExpired, "mcp.list", host.ID, msg).Err
		return mcpCatalogHostResult{
			host: agentapi.MCPCatalogHost{
				HostID:             host.ID,
				MachineName:        name,
				Local:              host.local,
				State:              agentapi.MCPHostExpired,
				Freshness:          agentapi.MCPHostExpired,
				ObservedAt:         &observed,
				CachedServiceCount: len(entry.Services),
				Code:               diag.Code,
				Message:            diag.Message,
			},
			diags: []agentapi.Error{diag},
		}
	}
	services := convertMCPServices(host, name, entry.Services, agentapi.MCPHostStale, &observed, true)
	diag := agentapi.Fail(agentapi.CatalogStale, "mcp.list", host.ID, fmt.Sprintf("host unreachable; showing cached catalog from %s (%s)", observed.Format(time.RFC3339), causeMsg)).Err
	return mcpCatalogHostResult{
		host: agentapi.MCPCatalogHost{
			HostID:             host.ID,
			MachineName:        name,
			Local:              host.local,
			State:              agentapi.MCPHostStale,
			Freshness:          agentapi.MCPHostStale,
			ObservedAt:         &observed,
			ServiceCount:       len(services),
			CachedServiceCount: len(services),
			Code:               diag.Code,
			Message:            diag.Message,
		},
		services: services,
		diags:    []agentapi.Error{diag},
		usable:   true,
	}
}

func convertMCPServices(host HostRecord, machineName string, views []mcphost.CatalogService, freshness string, observed *time.Time, stale bool) []agentapi.MCPCatalogService {
	out := make([]agentapi.MCPCatalogService, 0, len(views))
	for _, v := range views {
		state := classifyMCPServiceState(v)
		if stale {
			state = agentapi.MCPHostStale
		}
		out = append(out, convertOneMCPService(host, machineName, v, state, freshness, observed))
	}
	return out
}

func convertOneMCPService(host HostRecord, machineName string, v mcphost.CatalogService, state, freshness string, observed *time.Time) agentapi.MCPCatalogService {
	instances := make([]agentapi.MCPCatalogInstance, 0, len(v.Instances))
	for _, inst := range v.Instances {
		handle, _ := mcpconnect.FormatInstanceHandle(host.ID, v.ID, inst.ID)
		instances = append(instances, convertCatalogInstance(inst, handle))
	}
	return agentapi.MCPCatalogService{
		HostID:          host.ID,
		MachineName:     machineName,
		Local:           host.local,
		ID:              v.ID,
		Name:            v.Name,
		Revision:        v.Revision,
		State:           state,
		Freshness:       freshness,
		ObservedAt:      observed,
		Transport:       v.Transport,
		Lifecycle:       v.Lifecycle,
		IdleSeconds:     v.IdleSeconds,
		ReadySeconds:    v.ReadySeconds,
		InstancePolicy:  v.InstancePolicy,
		AccountMode:     v.AccountMode,
		AccountLabel:    v.AccountLabel,
		Disabled:        v.Disabled,
		ProtocolVersion: v.ProtocolVersion,
		Instances:       instances,
	}
}

func convertCatalogInstance(inst mcphost.CatalogInstance, handle string) agentapi.MCPCatalogInstance {
	return agentapi.MCPCatalogInstance{
		ID:           inst.ID,
		Handle:       handle,
		Shared:       inst.Shared,
		Mine:         inst.Mine,
		Default:      inst.Default,
		State:        inst.State,
		Reason:       inst.Reason,
		ExitCode:     inst.ExitCode,
		Revision:     inst.Revision,
		Generation:   inst.Generation,
		Active:       inst.Active,
		Leases:       inst.Leases,
		ServerInfo:   toAgentServerInfo(inst.ServerInfo),
		Capabilities: toAgentCapabilities(inst.Capabilities),
		LateReplies:  inst.LateReplies,
		UpdatedAt:    inst.UpdatedAt,
	}
}

func toAgentServerInfo(in *mcphost.CatalogServerInfo) *agentapi.MCPServerInfo {
	if in == nil {
		return nil
	}
	return &agentapi.MCPServerInfo{Name: in.Name, Version: in.Version, Title: in.Title}
}

func toAgentCapabilities(in *mcphost.CatalogCapabilities) *agentapi.MCPCapabilities {
	if in == nil {
		return nil
	}
	out := &agentapi.MCPCapabilities{Completions: in.Completions, Logging: in.Logging}
	if in.Tools != nil {
		out.Tools = &agentapi.MCPListCapability{ListChanged: in.Tools.ListChanged}
	}
	if in.Prompts != nil {
		out.Prompts = &agentapi.MCPListCapability{ListChanged: in.Prompts.ListChanged}
	}
	if in.Resources != nil {
		out.Resources = &agentapi.MCPResourceCapability{Subscribe: in.Resources.Subscribe, ListChanged: in.Resources.ListChanged}
	}
	return out
}

func classifyMCPServiceState(v mcphost.CatalogService) string {
	if v.Disabled {
		return mcpServiceDisabled
	}
	if len(v.Instances) == 0 {
		return mcphost.StateStopped
	}
	priority := []string{
		mcphost.StateReady,
		mcphost.StateStarting,
		mcphost.StateIncompatible,
		mcphost.StateFailed,
		mcphost.StateUncertain,
		mcphost.StateInterrupted,
		mcphost.StateStopped,
	}
	present := map[string]bool{}
	for _, inst := range v.Instances {
		present[inst.State] = true
	}
	for _, candidate := range priority {
		if present[candidate] {
			return candidate
		}
	}
	return v.Instances[0].State
}

func buildMCPCatalogData(now time.Time, results []mcpCatalogHostResult, extra []agentapi.Error) (agentapi.MCPCatalogData, []agentapi.Error, int) {
	data := agentapi.MCPCatalogData{
		ObservedAt: now,
		Hosts:      make([]agentapi.MCPCatalogHost, 0, len(results)),
		Services:   []agentapi.MCPCatalogService{},
	}
	diags := append([]agentapi.Error{}, extra...)
	usable := 0
	for _, r := range results {
		data.Hosts = append(data.Hosts, r.host)
		data.Services = append(data.Services, r.services...)
		diags = append(diags, r.diags...)
		if r.usable {
			usable++
		}
	}
	slices.SortFunc(data.Services, func(a, b agentapi.MCPCatalogService) int {
		if c := strings.Compare(a.HostID, b.HostID); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return data, diags, usable
}

func writeMCPFleetTable(w io.Writer, data agentapi.MCPCatalogData) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "HOST\tNAME\tSTATE\tTRANSPORT\tPOLICY\tLIFECYCLE\tACCOUNT\tINSTANCES")
	for _, s := range data.Services {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			mcpHostLabel(s.HostID, s.MachineName, s.Local),
			s.Name, s.State, s.Transport, s.InstancePolicy, s.Lifecycle,
			mcpAccountText(s.AccountMode, s.AccountLabel),
			mcpInstanceSummary(s.Instances),
		)
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write MCP services: %w", err)
	}
	var notable []agentapi.MCPCatalogHost
	for _, h := range data.Hosts {
		if h.State != agentapi.MCPHostOnline {
			notable = append(notable, h)
		}
	}
	if len(notable) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nHosts:"); err != nil {
		return fmt.Errorf("write host status header: %w", err)
	}
	for _, h := range notable {
		label := mcpHostLabel(h.HostID, h.MachineName, h.Local)
		if h.Message != "" {
			if _, err := fmt.Fprintf(w, "  %s\t%s\t%s\n", label, h.State, h.Message); err != nil {
				return fmt.Errorf("write host status: %w", err)
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "  %s\t%s\n", label, h.State); err != nil {
			return fmt.Errorf("write host status: %w", err)
		}
	}
	return nil
}

func mcpHostLabel(hostID, machineName string, local bool) string {
	if machineName != "" {
		return machineName
	}
	if local {
		return mcpLocalHost
	}
	if len(hostID) > 12 {
		return hostID[:12]
	}
	return hostID
}

func mcpAccountText(mode, label string) string {
	if mode == mcphost.AccountHostAccount {
		return label
	}
	return mode
}

func mcpInstanceSummary(instances []agentapi.MCPCatalogInstance) string {
	if len(instances) == 0 {
		return mcpFreshnessNone
	}
	states := map[string]int{}
	for _, inst := range instances {
		states[inst.State]++
	}
	keys := make([]string, 0, len(states))
	for k := range states {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, state := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", states[state], state))
	}
	return strings.Join(parts, ", ")
}
