package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

type mcpControls struct {
	app     *application
	pool    *mcpHostPool
	caller  protocol.CallerCredential
	account string
}

func newMCPControls(app *application, pool *mcpHostPool, caller protocol.CallerCredential) *mcpControls {
	return &mcpControls{
		app:     app,
		pool:    pool,
		caller:  caller,
		account: currentAccountName(),
	}
}

func currentAccountName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

func (c *mcpControls) targetAccount(host HostRecord, info protocol.HostInfo) string {
	if info.Account != "" {
		return info.Account
	}
	if host.local {
		return c.account
	}
	return ""
}

func (c *mcpControls) targetMeta(host HostRecord, info protocol.HostInfo) *mcpnative.TargetMeta {
	return &mcpnative.TargetMeta{
		HostID:   host.ID,
		HostName: hostDisplayName(host, info),
		Account:  c.targetAccount(host, info),
	}
}

func hostCapabilitiesFromInfo(info protocol.HostInfo) *agentapi.HostCapabilities {
	return &agentapi.HostCapabilities{
		ExecutionPipe: info.ExecutionPipeSupported,
	}
}

func (c *mcpControls) ListHosts(ctx context.Context, req mcpnative.ListHostsRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	reader, err := newAgentHostReader("hosts.list", false, req.Cached, c.app)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	local, err := agentLocalHost(reader.state)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	hosts = agentHostsWithLocal(hosts, local)
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	results := collectMCPHosts(ctx, hosts, reader, c.pool)
	authority := &update.Authority{StateDir: reader.state, ID: local.ID}
	fleet, _ := hostprofile.OpenFleetStore(reader.state).ReadReplica(authority.IsAdministrator)
	now := c.app.dependencies.Now().UTC()
	data := agentapi.HostsData{
		ReceivedAt: now,
		Fleet:      contextDeclared(hostprofile.ContextReply{Fleet: hostprofile.FleetSection(fleet, true)}, agentapi.FreshnessLive, now).Fleet,
		Hosts:      make([]agentapi.HostSummary, len(results)),
	}
	diagnostics := []agentapi.Error{}
	for i, result := range results {
		data.Hosts[i] = result.host
		diagnostics = append(diagnostics, result.errors...)
	}
	if fleet.State == hostprofile.StateUnavailable {
		diagnostics = append(diagnostics, reader.diagnostic(errors.New(fleet.Problem), local.ID, agentapi.StateUnavailable))
	}
	if !req.Cached && local.ID != "" {
		if err := pruneAgentContext(ctx, reader, hosts); err != nil {
			diagnostics = append(diagnostics, reader.diagnostic(err, "", agentapi.StateUnavailable))
		}
	}
	status := agentapi.StatusOK
	if len(diagnostics) > 0 {
		status = agentapi.StatusPartial
	}
	return mcpnative.CallOutcome{
		Envelope: agentapi.Envelope{Operation: "hosts.list", Status: status, Data: data, Errors: diagnostics},
	}, nil
}

func collectMCPHosts(ctx context.Context, hosts []HostRecord, reader agentHostReader, pool *mcpHostPool) []agentHostRead {
	channel := make(chan indexedAgentHost, len(hosts))
	for i, host := range hosts {
		go func() { channel <- indexedAgentHost{index: i, result: readMCPHost(ctx, host, reader, pool)} }()
	}
	results := make([]agentHostRead, len(hosts))
	for range hosts {
		result := <-channel
		results[result.index] = result.result
	}
	return results
}

func readMCPHost(ctx context.Context, host HostRecord, reader agentHostReader, pool *mcpHostPool) agentHostRead {
	result, _ := readMCPHostWithInfo(ctx, host, reader, pool)
	return result
}

func readMCPHostWithInfo(ctx context.Context, host HostRecord, reader agentHostReader, pool *mcpHostPool) (agentHostRead, protocol.HostInfo) {
	if host.local {
		return readMCPLocalHost(ctx, host, reader, pool)
	}
	cache := hostprofile.OpenContextCache(reader.state)
	entry, cacheErr := cache.Read(host.ID, host.MeshIdentity)
	result := reader.cachedResult(host, entry)
	if cacheErr != nil {
		result.errors = append(result.errors, reader.diagnostic(cacheErr, host.ID, agentapi.StateUnavailable))
	}
	if reader.cached {
		return reader.finish(result), protocol.HostInfo{}
	}
	reply, info, err := pool.HostContext(ctx, host, reader.full)
	at := reader.now().UTC()
	var observation hostprofile.ContextObservation
	var observedInfo protocol.HostInfo
	if info != nil {
		observedInfo = *info
		observation = hostObservation(*info)
		result.host.MachineName, result.host.NameShadowed = info.MachineName, machinename.ShadowedByCommand(info.MachineName)
		result.host.Reachability, result.host.Observed = agentReachable, agentObservation(&observation, at, agentapi.FreshnessLive)
		result.host.Capabilities = hostCapabilitiesFromInfo(*info)
		result.useful = true
	}
	if err != nil {
		if info == nil {
			result.host.Reachability = "unreachable"
		}
		result.errors = append(result.errors, reader.diagnostic(err, host.ID, agentapi.HostUnreachable))
		return reader.finish(result), observedInfo
	}
	result.host.Declared = contextDeclared(reply, agentapi.FreshnessLive, at)
	result.liveAccess = true
	result.errors = append(result.errors, reader.contextProblems(reply, host.ID)...)
	reader.cacheLive(ctx, host, *info, hostprofile.CachedContext{HostID: host.ID, MeshIdentity: host.MeshIdentity, ReceivedAt: at, Observed: &observation, Context: reply}, &result)
	return reader.finish(result), observedInfo
}

func readMCPLocalHost(ctx context.Context, host HostRecord, reader agentHostReader, pool *mcpHostPool) (agentHostRead, protocol.HostInfo) {
	result := agentHostRead{host: agentHostBase(host), useful: true}
	if host.ID == "" {
		result.host.Reachability = "uninitialized"
		result.errors = append(result.errors, reader.diagnostic(errors.New("This machine has no Mesh identity yet. Set up Mesh here first with mesh install."), "", agentapi.HostUninitialized))
		return result, protocol.HostInfo{}
	}
	authority := &update.Authority{StateDir: reader.state, ID: host.ID}
	reply := hostprofile.ReadContext(reader.config, reader.state, host.ID, authority.IsAdministrator, reader.full)
	result.host.Declared = contextDeclared(reply, agentapi.FreshnessLive, reader.now().UTC())
	result.errors = reader.contextProblems(reply, host.ID)
	if reader.cached {
		return result, protocol.HostInfo{}
	}
	probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	info, err := pool.HostInfo(probe, host)
	if err != nil {
		applyLocalProbeFailure(&result, host, reader, err)
		return result, protocol.HostInfo{}
	}
	if err := validateContextObservation(info); err != nil {
		result.errors = append(result.errors, reader.diagnostic(err, host.ID, agentapi.HostUnreachable))
		return result, info
	}
	observation := hostObservation(info)
	result.host.Observed = agentObservation(&observation, reader.now().UTC(), agentapi.FreshnessLive)
	result.host.Capabilities = hostCapabilitiesFromInfo(info)
	result.host.Reachability, result.liveAccess = agentReachable, true
	return result, info
}

func applyLocalProbeFailure(result *agentHostRead, host HostRecord, reader agentHostReader, err error) {
	at := reader.now().UTC()
	daemon := agentUnknown
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		daemon = "not-running"
	}
	result.host.Observed = &agentapi.Observed{Platform: release.CurrentPlatform().String(), Daemon: daemon, ObservedAt: at, Freshness: agentapi.FreshnessLive}
	result.host.Reachability = agentReachable
	if daemon == agentUnknown {
		result.errors = append(result.errors, reader.diagnostic(err, host.ID, agentapi.HostUnreachable))
	}
}

func (c *mcpControls) InspectHost(ctx context.Context, req mcpnative.InspectHostRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	reader, err := newAgentHostReader("hosts.inspect", true, req.Cached, c.app)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	host, err := c.resolveMCPHost(ctx, req.Host, hosts, req.Cached)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	result, info := readMCPHostWithInfo(ctx, host, reader, c.pool)
	if !result.useful && len(result.errors) == 0 {
		result.errors = append(result.errors, reader.diagnostic(errors.New("No context observation is cached for this machine."), host.ID, agentapi.StateUnavailable))
	}
	target := &mcpnative.TargetMeta{HostID: host.ID, HostName: result.host.MachineName, Account: c.targetAccount(host, info)}
	envelope := agentapi.Envelope{
		Operation: "hosts.inspect",
		Status:    agentapi.StatusOK,
		Data:      agentapi.HostDetail{Host: result.host, Access: reader.access(result, host)},
		Errors:    result.errors,
	}
	if len(result.errors) > 0 {
		envelope.Status = agentapi.StatusPartial
	}
	if !result.useful {
		envelope.Status, envelope.Data = agentapi.StatusError, nil
	}
	return mcpnative.CallOutcome{Envelope: envelope, Target: target, IsError: envelope.Status == agentapi.StatusError}, nil
}

func (c *mcpControls) WakeHost(ctx context.Context, req mcpnative.WakeHostRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	hosts, err := LoadHosts()
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	host, err := c.resolveMCPHost(ctx, req.Host, hosts, true)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	target := c.targetMeta(host, protocol.HostInfo{})
	if c.app.dependencies.Wake != nil {
		if err := c.app.dependencies.Wake(ctx, host); err != nil {
			return failOutcome("hosts.wake", target, agentError(err, "hosts.wake", host.ID, agentapi.HostUnreachable)), nil
		}
		return okOutcome("hosts.wake", target, mcpnative.WakeResult{HostID: host.ID, MachineName: host.MachineName}), nil
	}
	client, err := newWakeClient()
	if err != nil {
		return failOutcome("hosts.wake", target, agentError(err, "hosts.wake", host.ID, agentapi.StateUnavailable)), nil
	}
	result, err := client.Wake(ctx, wakeTarget(host))
	if err != nil {
		return failOutcome("hosts.wake", target, agentError(err, "hosts.wake", host.ID, agentapi.HostUnreachable)), nil
	}
	return okOutcome("hosts.wake", target, mcpnative.WakeResult{
		HostID:        host.ID,
		MachineName:   host.MachineName,
		AlreadyOnline: result.AlreadyOnline,
		Sender:        result.Sender,
	}), nil
}

func (c *mcpControls) ListSessions(ctx context.Context, req mcpnative.ListSessionsRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cursor, err := parseAgentSessionCursor(req.Page)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	host, err := c.resolveSessionHost(ctx, req.Host)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	target := c.targetMeta(host, protocol.HostInfo{})
	if cursor != nil && cursor.HostID != host.ID {
		return failOutcome(agentSessionsList, target, agentapi.Usage(agentSessionsList, "page belongs to another host.").Err), nil
	}
	snapshot, err := c.loadSessions(ctx, host, agentSessionsList)
	if err != nil {
		return failOutcome(agentSessionsList, target, agentError(err, agentSessionsList, host.ID, agentapi.HostUnreachable)), nil
	}
	options := agentSessionOptions{host: req.Host, all: req.All, detail: req.Detail, limit: req.Limit, page: req.Page}
	page := agentSessionPage(host.ID, snapshot, options, cursor)
	return mcpnative.CallOutcome{Envelope: envelopeWithOp(agentSessionsList, agentSessionEnvelope(page, snapshot.errors)), Target: target}, nil
}

func (c *mcpControls) InspectSession(ctx context.Context, req mcpnative.InspectSessionRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveSessionHost(ctx, req.Host)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	target := c.targetMeta(host, protocol.HostInfo{})
	snapshot, err := c.loadSessions(ctx, host, agentSessionsInspect)
	if err != nil {
		return failOutcome(agentSessionsInspect, target, agentError(err, agentSessionsInspect, host.ID, agentapi.HostUnreachable)), nil
	}
	row, found := findAgentSession(snapshot.rows, req.SessionID)
	if !found && len(snapshot.errors) > 0 {
		return failOutcome(agentSessionsInspect, target, snapshot.errors[0]), nil
	}
	if !found {
		return failOutcome(agentSessionsInspect, target, agentapi.Fail(agentapi.SessionNotFound, agentSessionsInspect, req.SessionID, "No such session on this host.").Err), nil
	}
	result := agentapi.SessionDetail{Session: agentSessionSummary(row, true), Freshness: snapshot.freshness, LiveState: agentUnavailable}
	if snapshot.freshness == agentapi.FreshnessCached {
		return mcpnative.CallOutcome{Envelope: envelopeWithOp(agentSessionsInspect, agentSessionEnvelope(result, snapshot.errors)), Target: target}, nil
	}
	cols, lines := 1, 1
	if req.Preview {
		cols, lines = 80, 24
	}
	inspection, err := c.inspectSession(ctx, host, req.SessionID, cols, lines)
	if err != nil {
		failure := agentError(err, agentSessionsInspect, req.SessionID, agentapi.HostUnreachable)
		if failure.Code == agentapi.SessionNotFound || failure.Code == agentapi.HostIdentityMismatch || failure.Code == agentapi.HostAuthenticationFailed {
			return failOutcome(agentSessionsInspect, target, failure), nil
		}
		snapshot.errors = append(snapshot.errors, failure)
	} else {
		applyAgentInspection(&result, inspection, req.Preview)
	}
	return mcpnative.CallOutcome{Envelope: envelopeWithOp(agentSessionsInspect, agentSessionEnvelope(result, snapshot.errors)), Target: target}, nil
}

func (c *mcpControls) ListServices(ctx context.Context, req mcpnative.ListServicesRequest) (mcpnative.CallOutcome, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	var target *mcpnative.TargetMeta
	if strings.TrimSpace(req.Host) != "" {
		resolveCtx, cancel := context.WithTimeout(ctx, req.Timeout)
		defer cancel()
		host, err := c.resolveMCPHost(resolveCtx, req.Host, hosts, false)
		if err != nil {
			return mcpnative.CallOutcome{}, err
		}
		hosts = []HostRecord{host}
		target = c.targetMeta(host, protocol.HostInfo{})
	}
	envelope, err := c.collectServices(ctx, hosts, req.Timeout)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	return mcpnative.CallOutcome{Envelope: envelope, Target: target, IsError: envelope.Status == agentapi.StatusError}, nil
}

func (c *mcpControls) InspectService(ctx context.Context, req mcpnative.InspectServiceRequest) (mcpnative.CallOutcome, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	resolveCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveMCPHost(resolveCtx, req.Host, hosts, false)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	target := c.targetMeta(host, protocol.HostInfo{})
	envelope, err := c.collectServices(ctx, []HostRecord{host}, req.Timeout)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	data, ok := envelope.Data.(agentapi.ServicesData)
	if !ok {
		envelope.Operation = "services.inspect"
		return mcpnative.CallOutcome{Envelope: envelope, Target: target, IsError: true}, nil
	}
	for _, row := range data.Services {
		if row.HostID == host.ID && row.Name == req.Name {
			status := agentapi.StatusOK
			if len(envelope.Errors) > 0 {
				status = agentapi.StatusPartial
			}
			return mcpnative.CallOutcome{
				Envelope: agentapi.Envelope{Operation: "services.inspect", Status: status, Data: mcpnative.ServiceDetail{Service: row}, Errors: envelope.Errors},
				Target:   target,
			}, nil
		}
	}
	return failOutcome("services.inspect", target, agentapi.Fail(agentapi.ServiceNotFound, "services.inspect", req.Name, fmt.Sprintf("No service named %q on host %s.", req.Name, host.ID)).Err), nil
}

func (c *mcpControls) collectServices(ctx context.Context, hosts []HostRecord, timeout time.Duration) (agentapi.Envelope, error) {
	if len(hosts) == 0 {
		return agentapi.Envelope{
			Operation: "services.list",
			Status:    agentapi.StatusOK,
			Data:      agentapi.ServicesData{Services: []agentapi.ServiceRow{}, HostErrors: []agentapi.Error{}},
			Errors:    []agentapi.Error{},
		}, nil
	}
	hosts, err := agentServiceTargets(ctx, hosts)
	if err != nil {
		return agentapi.Envelope{}, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cache, cacheErr := openAgentCatalogCache(queryCtx)
	if cache == nil {
		cache = &SQLiteCatalogCache{}
	}
	defer cache.Close() //nolint:errcheck // read errors are reported separately
	adapter := &agentServiceCache{cache: cache, err: cacheErr}
	deadline, _ := queryCtx.Deadline()
	if time.Until(deadline) <= 0 {
		return agentapi.Envelope{}, agentapi.Fail(agentapi.StateUnavailable, "services.list", "", "Service cache read exceeded the deadline.")
	}
	rows, diagnostics, err := collectServiceCatalogBy(ctx, hosts, deadline, c.listHostServices, adapter)
	if err != nil {
		return agentapi.Envelope{}, err
	}
	if adapter.err != nil && !serviceCacheWarningReported(diagnostics) {
		diagnostics[""] = agentapi.Fail(agentapi.StateUnavailable, "services.list", "", adapter.err.Error())
	}
	return envelopeWithOp("services.list", servicesEnvelope(rows, diagnostics, hosts, c.app.dependencies.Now().UTC())), nil
}

func (c *mcpControls) listHostServices(ctx context.Context, host HostRecord) (remoteServiceSnapshot, error) {
	token, err := loadedServicePolicy(ctx)
	if err != nil {
		return remoteServiceSnapshot{}, err
	}
	response, info, err := c.pool.ExchangeShort(ctx, host, protocol.Control{Type: protocol.TypeServiceList}, true)
	if err != nil {
		return remoteServiceSnapshot{}, err
	}
	snapshot, err := decodeRemoteServices(host, response, info)
	snapshot.Policy = token
	return snapshot, err
}

func (c *mcpControls) JobStatus(ctx context.Context, req mcpnative.JobRefRequest) (mcpnative.CallOutcome, error) {
	return c.runJobStateControl(ctx, "sessions.inspect", req.Job.HostID, req.Timeout, protocol.Control{
		Type:      protocol.TypeExecutionInspect,
		SessionID: req.Job.SessionID,
		Instance:  req.Job.Instance,
	})
}

func (c *mcpControls) JobWait(ctx context.Context, req mcpnative.JobWaitRequest, notify func(mcpnative.ProgressUpdate)) (mcpnative.CallOutcome, error) {
	deadline := time.Now().Add(time.Duration(req.WaitMS) * time.Millisecond)
	host, err := c.resolveExactHostID(req.Job.HostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	stopProgress := startWaitProgress(ctx, req.Job, req.WaitMS, notify)
	defer stopProgress()

	var sentControl protocol.Control
	response, info, err := c.pool.ExchangeStreamBudgeted(ctx, host, deadline, func(timeoutMS int) protocol.Control {
		sentControl = protocol.Control{
			Type:          protocol.TypeExecutionWait,
			Caller:        c.caller,
			SessionID:     req.Job.SessionID,
			Instance:      req.Job.Instance,
			TimeoutMillis: timeoutMS,
		}
		return sentControl
	})
	target := c.targetMeta(host, info)
	if err != nil {
		return failOutcome("sessions.wait", target, agentError(err, "sessions.wait", host.ID, agentapi.HostUnreachable)), nil
	}
	return c.jobStateOutcome("sessions.wait", sentControl, response, host.ID, "", target)
}

func startWaitProgress(ctx context.Context, ref mcpnative.JobRef, waitMS int, notify func(mcpnative.ProgressUpdate)) func() {
	if notify == nil {
		return func() {}
	}
	done := make(chan struct{})
	total := float64(waitMS)
	started := time.Now()
	notify(mcpnative.ProgressUpdate{Progress: 0, Total: &total, Message: "waiting for " + ref.String()})
	go runWaitProgressLoop(ctx, done, ref, started, total, notify)
	return func() { close(done) }
}

func runWaitProgressLoop(ctx context.Context, done <-chan struct{}, ref mcpnative.JobRef, started time.Time, total float64, notify func(mcpnative.ProgressUpdate)) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			elapsed := min(float64(time.Since(started)/time.Millisecond), total)
			notify(mcpnative.ProgressUpdate{Progress: elapsed, Total: &total, Message: "waiting for " + ref.String()})
		}
	}
}

func (c *mcpControls) JobOutput(ctx context.Context, req mcpnative.JobOutputRequest) (mcpnative.CallOutcome, error) {
	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveExactHostID(req.Job.HostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	control := protocol.Control{
		Type:      protocol.TypeExecutionLogs,
		Caller:    c.caller,
		SessionID: req.Job.SessionID,
		Instance:  req.Job.Instance,
		Stream:    req.Stream,
		Cursor:    req.Cursor,
		MaxBytes:  req.MaxBytes,
	}
	response, info, err := c.pool.ExchangeShort(callCtx, host, control, false)
	target := c.targetMeta(host, info)
	if err != nil {
		return failOutcome("sessions.logs", target, agentError(err, "sessions.logs", host.ID, agentapi.HostUnreachable)), nil
	}
	return c.jobOutputOutcome(control, response, host.ID, target)
}

func (c *mcpControls) jobOutputOutcome(control, response protocol.Control, hostID string, target *mcpnative.TargetMeta) (mcpnative.CallOutcome, error) {
	var pageData *mcpnative.JobOutputData
	if response.ExecutionPage != nil {
		page, err := agentExecutionPage(response.ExecutionPage, hostID, control)
		if err != nil {
			return failOutcome("sessions.logs", target, agentError(err, "sessions.logs", hostID, agentapi.InternalError)), nil
		}
		built := mcpnative.BuildJobOutputData(page, *target)
		pageData = &built
	}
	if response.Type == protocol.TypeError {
		failure := agentapi.Fail(controlAgentCode(response.ErrorCode), "sessions.logs", hostID, response.Message).Err
		envelope := agentapi.Envelope{Operation: "sessions.logs", Status: agentapi.StatusError, Errors: []agentapi.Error{failure}}
		if pageData != nil {
			envelope.Data = *pageData
		}
		return mcpnative.CallOutcome{Envelope: envelope, Target: target, IsError: true}, nil
	}
	if response.Type != protocol.TypeExecutionReceipt || pageData == nil {
		return failOutcome("sessions.logs", target, agentapi.Fail(agentapi.InternalError, "sessions.logs", hostID, "host did not return an output page").Err), nil
	}
	return okOutcome("sessions.logs", target, *pageData), nil
}

func (c *mcpControls) JobReceipt(ctx context.Context, req mcpnative.JobReceiptRequest) (mcpnative.CallOutcome, error) {
	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveSessionHost(callCtx, req.Host)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	control := protocol.Control{
		Type:       protocol.TypeExecutionRequest,
		Caller:     c.caller,
		RequestKey: req.RequestKey,
	}
	response, info, err := c.pool.ExchangeShort(callCtx, host, control, false)
	target := c.targetMeta(host, info)
	if err != nil {
		return failOutcome("sessions.request", target, agentError(err, "sessions.request", host.ID, agentapi.HostUnreachable)), nil
	}
	return c.jobStateOutcome("sessions.request", control, response, host.ID, req.RequestKey, target)
}

func (c *mcpControls) ListJobs(ctx context.Context, req mcpnative.ListJobsRequest) (mcpnative.CallOutcome, error) {
	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveSessionHost(callCtx, req.Host)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	control := protocol.Control{
		Type:               protocol.TypeExecutionList,
		Caller:             c.caller,
		ExecutionListAll:   req.All,
		ExecutionListLimit: req.Limit,
		Cursor:             req.Page,
	}
	response, info, err := c.pool.ExchangeShort(callCtx, host, control, true)
	target := c.targetMeta(host, info)
	if err != nil {
		return failOutcome("jobs.list", target, agentError(err, "jobs.list", host.ID, agentapi.HostUnreachable)), nil
	}
	if response.Type == protocol.TypeError {
		return failOutcome("jobs.list", target, agentapi.Fail(controlAgentCode(response.ErrorCode), "jobs.list", host.ID, response.Message).Err), nil
	}
	if response.Type != protocol.TypeExecutionListed {
		return failOutcome("jobs.list", target, agentapi.Fail(agentapi.InternalError, "jobs.list", host.ID, "host did not return an execution job list").Err), nil
	}
	data := buildExecutionJobListPage(host.ID, response.ExecutionReceipts, response.ExecutionNextPage)
	return okOutcome("jobs.list", target, data), nil
}

func buildExecutionJobListPage(hostID string, receipts []protocol.ExecutionReceipt, nextPage *string) mcpnative.JobListData {
	items := make([]mcpnative.JobListItem, 0, len(receipts))
	for i := range receipts {
		rec, err := agentExecutionReceipt(&receipts[i], hostID, "")
		if err != nil {
			continue
		}
		var exitCode *int
		if rec.Result != nil && rec.Result.Termination.Code != nil {
			code := *rec.Result.Termination.Code
			exitCode = &code
		}
		handle, err := mcpnative.FormatJobHandle(rec.HostID, rec.SessionID, rec.Instance)
		if err != nil {
			continue
		}
		items = append(items, mcpnative.JobListItem{
			Job:       handle,
			HostID:    rec.HostID,
			SessionID: rec.SessionID,
			Instance:  rec.Instance,
			State:     mcpnative.JobStateFromReceipt(rec),
			CreatedAt: rec.CreatedAt,
			ExitCode:  exitCode,
		})
	}
	return mcpnative.JobListData{
		HostID:    hostID,
		Freshness: agentapi.FreshnessLive,
		Jobs:      items,
		NextPage:  nextPage,
	}
}

func (c *mcpControls) JobInput(ctx context.Context, req mcpnative.JobInputRequest) (mcpnative.CallOutcome, error) {
	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveExactHostID(req.Job.HostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	control := protocol.Control{
		Type:        protocol.TypeExecutionInput,
		Caller:      c.caller,
		SessionID:   req.Job.SessionID,
		Instance:    req.Job.Instance,
		WriteKey:    req.WriteKey,
		InputOffset: req.Offset,
		InputBytes:  req.Data,
		InputEOF:    req.EOF,
	}
	response, info, err := c.pool.ExchangeShort(callCtx, host, control, false)
	target := c.targetMeta(host, info)
	if err != nil {
		if req.Offset != nil && !isPreDispatchError(err) {
			return failOutcome(agentSessionInput, target, agentapi.Fail(agentapi.InputOutcomeUnknown, agentSessionInput, host.ID, "Look up the write_key without offset to recover its receipt.").Err), nil
		}
		return failOutcome(agentSessionInput, target, agentError(err, agentSessionInput, host.ID, agentapi.HostUnreachable)), nil
	}
	if response.Type == protocol.TypeError {
		failure := agentapi.Fail(controlAgentCode(response.ErrorCode), agentSessionInput, host.ID, response.Message).Err
		return failOutcome(agentSessionInput, target, failure), nil
	}
	rawReceipt, err := agentExecutionInputData(response, control)
	if err != nil || rawReceipt == nil {
		return failOutcome(agentSessionInput, target, agentapi.Fail(agentapi.InternalError, agentSessionInput, host.ID, "host did not return a valid stdin receipt").Err), nil
	}
	receipt := response.InputReceipt
	return okOutcome(agentSessionInput, target, mcpnative.JobInputData{
		Job:      req.Job.String(),
		Target:   *target,
		WriteKey: receipt.WriteKey,
		Offset:   strconv.FormatUint(receipt.Offset, 10),
		Length:   receipt.Length,
		Closed:   receipt.Closed,
	}), nil
}

func (c *mcpControls) JobSignal(ctx context.Context, req mcpnative.JobSignalRequest) (mcpnative.CallOutcome, error) {
	return c.runJobStateControl(ctx, "sessions.signal", req.Job.HostID, req.Timeout, protocol.Control{
		Type:      protocol.TypeExecutionSignal,
		SessionID: req.Job.SessionID,
		Instance:  req.Job.Instance,
		Signal:    req.Signal,
	})
}

func (c *mcpControls) JobCancel(ctx context.Context, req mcpnative.JobRefRequest) (mcpnative.CallOutcome, error) {
	return c.runJobStateControl(ctx, "sessions.kill", req.Job.HostID, req.Timeout, protocol.Control{
		Type:      protocol.TypeExecutionKill,
		SessionID: req.Job.SessionID,
		Instance:  req.Job.Instance,
	})
}

func (c *mcpControls) JobRemove(ctx context.Context, req mcpnative.JobRefRequest) (mcpnative.CallOutcome, error) {
	return c.runJobStateControl(ctx, "sessions.remove", req.Job.HostID, req.Timeout, protocol.Control{
		Type:      protocol.TypeExecutionRemove,
		SessionID: req.Job.SessionID,
		Instance:  req.Job.Instance,
	})
}

func (c *mcpControls) runJobStateControl(ctx context.Context, operation, hostID string, timeout time.Duration, control protocol.Control) (mcpnative.CallOutcome, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, err := c.resolveExactHostID(hostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	if control.Caller == "" {
		control.Caller = c.caller
	}
	response, info, err := c.pool.ExchangeShort(callCtx, host, control, false)
	target := c.targetMeta(host, info)
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, host.ID, agentapi.HostUnreachable)), nil
	}
	return c.jobStateOutcome(operation, control, response, host.ID, "", target)
}

func (c *mcpControls) jobStateOutcome(operation string, control, response protocol.Control, hostID, key string, target *mcpnative.TargetMeta) (mcpnative.CallOutcome, error) {
	var stateData *mcpnative.JobStateData
	if response.ExecutionReceipt != nil {
		receipt, err := agentExecutionReceipt(response.ExecutionReceipt, hostID, key)
		if err != nil {
			return failOutcome(operation, target, agentError(err, operation, hostID, agentapi.InternalError)), nil
		}
		if control.Instance != "" && (receipt.Instance != control.Instance || receipt.SessionID != control.SessionID) {
			return failOutcome(operation, target, agentapi.Fail(agentapi.SessionInstanceMismatch, operation, hostID, "host returned another execution instance").Err), nil
		}
		ref := mcpnative.JobRef{HostID: receipt.HostID, SessionID: receipt.SessionID, Instance: receipt.Instance}
		stateData = &mcpnative.JobStateData{
			Job:        ref.String(),
			RequestKey: receipt.RequestKey,
			Target:     *target,
			State:      mcpnative.JobStateFromReceipt(receipt),
			Wait:       response.ExecutionWait,
			Delivery:   response.ExecutionDelivery,
			Result:     receipt.Result,
			Receipt:    receipt,
		}
	}
	if response.Type == protocol.TypeError {
		failure := agentapi.Fail(controlAgentCode(response.ErrorCode), operation, hostID, response.Message).Err
		envelope := agentapi.Envelope{Operation: operation, Status: agentapi.StatusError, Errors: []agentapi.Error{failure}}
		if stateData != nil {
			envelope.Data = *stateData
		}
		return mcpnative.CallOutcome{Envelope: envelope, Target: target, IsError: true}, nil
	}
	if response.Type != protocol.TypeExecutionReceipt || stateData == nil {
		return failOutcome(operation, target, agentapi.Fail(agentapi.InternalError, operation, hostID, "host did not return an execution receipt").Err), nil
	}
	return okOutcome(operation, target, *stateData), nil
}

// resolveExactHostID resolves a host strictly by its exact host ID and never
// falls back to machine names.
func (c *mcpControls) resolveExactHostID(hostID string) (HostRecord, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return HostRecord{}, err
	}
	state, err := paths.StateDirPath()
	if err != nil {
		return HostRecord{}, fmt.Errorf("locate passive target state: %w", err)
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return HostRecord{}, err
	}
	if local.ID != "" {
		hosts = agentHostsWithLocal(hosts, local)
	}
	for _, host := range hosts {
		if host.ID != "" && host.ID == hostID {
			return host, nil
		}
	}
	return HostRecord{}, agentapi.Fail(agentapi.HostUnknown, "", hostID, "No configured host has this exact host ID.")
}

func (c *mcpControls) resolveSessionHost(ctx context.Context, value string) (HostRecord, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return HostRecord{}, err
	}
	return c.resolveMCPHost(ctx, value, hosts, false)
}

// resolveMCPHost resolves a machine name or exact host ID, routing any passive
// name refresh through the per-host pool instead of raw unpooled dials.
func (c *mcpControls) resolveMCPHost(ctx context.Context, value string, hosts []HostRecord, cached bool) (HostRecord, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return HostRecord{}, fmt.Errorf("locate passive target state: %w", err)
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return HostRecord{}, err
	}
	known := append([]HostRecord(nil), hosts...)
	if local.ID != "" {
		known = agentHostsWithLocal(known, local)
	}
	host, err := matchAgentHost(value, known)
	var failure *agentapi.Failure
	if err == nil || cached || local.ID == "" || !errors.As(err, &failure) || failure.Err.Code != agentapi.HostUnknown {
		return host, err
	}
	if _, finite := ctx.Deadline(); !finite {
		return HostRecord{}, errors.New("passive name refresh requires a finite deadline")
	}
	refreshed := c.refreshMCPHostNames(ctx, known)
	return matchAgentHost(value, refreshed)
}

func (c *mcpControls) refreshMCPHostNames(ctx context.Context, hosts []HostRecord) []HostRecord {
	results := make(chan HostRecord, len(hosts))
	for _, host := range hosts {
		go c.refreshOneMCPHostName(ctx, host, results)
	}
	refreshed := make([]HostRecord, 0, len(hosts))
	for range hosts {
		select {
		case host := <-results:
			refreshed = append(refreshed, host)
		case <-ctx.Done():
			return refreshed
		}
	}
	return refreshed
}

func (c *mcpControls) refreshOneMCPHostName(ctx context.Context, host HostRecord, results chan<- HostRecord) {
	if host.local {
		results <- host
		return
	}
	host.targetName = ""
	if info, err := c.pool.HostInfo(ctx, host); err == nil {
		host.MachineName, host.NameRevision, host.NameVerified = info.MachineName, info.NameRevision, true
	}
	results <- host
}

func (c *mcpControls) loadSessions(ctx context.Context, host HostRecord, operation string) (agentSessionSnapshot, error) {
	if host.local {
		rows, err := listAgentLocalSessions(ctx, host)
		if err != nil {
			return agentSessionSnapshot{}, err
		}
		return agentSessionSnapshot{rows: liveAgentSessionRows(rows), freshness: agentapi.FreshnessLive}, nil
	}
	return c.app.loadAgentRemoteSessionsBy(ctx, host, operation, c.listRemoteSessions)
}

func (c *mcpControls) listRemoteSessions(ctx context.Context, host HostRecord) ([]protocol.SessionInfo, error) {
	response, _, err := c.pool.ExchangeShort(ctx, host, protocol.Control{Type: protocol.TypeList, Lean: true}, true)
	if err != nil {
		return nil, err
	}
	if response.Type == protocol.TypeError {
		return nil, daemonResponseError("session list", response.Message, response.ErrorCode)
	}
	if response.Type != protocol.TypeListed {
		return nil, errors.New("host returned an unexpected session-list response")
	}
	if err := validateAgentSessionRows(host.ID, response.Sessions); err != nil {
		return nil, err
	}
	return response.Sessions, nil
}

func (c *mcpControls) inspectSession(ctx context.Context, host HostRecord, id string, cols, rows int) (SessionInspection, error) {
	if host.local {
		return inspectAgentLocalSession(ctx, host, id, cols, rows)
	}
	response, _, err := c.pool.ExchangeShort(ctx, host, protocol.Control{
		Type: protocol.TypeInspect, SessionID: id, PreviewCols: cols, PreviewRows: rows,
	}, true)
	if err != nil {
		return SessionInspection{}, err
	}
	return validateInspectionResponse(host, id, cols, rows, response)
}

func hostDisplayName(host HostRecord, info protocol.HostInfo) string {
	if info.MachineName != "" {
		return info.MachineName
	}
	return host.MachineName
}

func okOutcome(operation string, target *mcpnative.TargetMeta, data any) mcpnative.CallOutcome {
	return mcpnative.CallOutcome{
		Envelope: agentapi.Envelope{Operation: operation, Status: agentapi.StatusOK, Data: data, Errors: []agentapi.Error{}},
		Target:   target,
	}
}

func failOutcome(operation string, target *mcpnative.TargetMeta, err agentapi.Error) mcpnative.CallOutcome {
	return mcpnative.CallOutcome{
		Envelope: agentapi.Envelope{Operation: operation, Status: agentapi.StatusError, Errors: []agentapi.Error{err}},
		Target:   target,
		IsError:  true,
	}
}

func envelopeWithOp(operation string, envelope agentapi.Envelope) agentapi.Envelope {
	envelope.Operation = operation
	return envelope
}
