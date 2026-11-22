package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	appspkg "github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

// maxServiceWarnings bounds the host warnings one service result carries.
const maxServiceWarnings = 8

const (
	appLocalHost  = "local"
	appActionList = "list"
	// recoveryTerm is the terminal type a detached replacement starts
	// under; the person who attaches later brings their own size.
	recoveryTerm = "xterm-256color"
)

// poolServiceExchange sends service controls over the frontend's pooled,
// pinned connection and remembers the host's private name for URLs.
func (c *mcpControls) poolServiceExchange(host HostRecord, last *protocol.HostInfo) serviceExchange {
	return func(ctx context.Context, request protocol.Control, expectedPrivateName *string) (protocol.Control, protocol.HostInfo, error) {
		if expectedPrivateName == nil {
			response, info, err := c.pool.ExchangeShort(ctx, host, request, request.Type == protocol.TypeServiceList)
			*last = info
			return response, info, err
		}
		// The private name the preview used must still hold before the
		// publish is sent, on the connection that sends it.
		check := func(info protocol.HostInfo) error {
			if info.PrivateName != *expectedPrivateName {
				return fmt.Errorf("host %s private name changed after preview", HostLabel(host))
			}
			return nil
		}
		response, info, err := c.pool.ExchangeShortChecked(ctx, host, request, check)
		*last = info
		return response, info, err
	}
}

// serviceCall resolves the host and runs one service operation, turning a
// failure into the shared bounded error envelope.
func (c *mcpControls) serviceCall(ctx context.Context, operation, hostArg string, timeout time.Duration, run func(context.Context, HostRecord, serviceExchange) (any, error)) (mcpnative.CallOutcome, error) {
	// Passive discovery mode never changes a host.
	if c.caller == "" {
		return c.identityRequiredOutcome(operation, ""), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, err := c.resolveSessionHost(callCtx, hostArg)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	var info protocol.HostInfo
	data, err := run(callCtx, host, c.poolServiceExchange(host, &info))
	target := c.targetMeta(host, info)
	if err != nil {
		return failOutcome(operation, target, boundedAgentError(err, operation, host.ID)), nil
	}
	return okOutcome(operation, target, data), nil
}

// boundedAgentError keeps host-supplied text short and printable.
func boundedAgentError(err error, operation, target string) agentapi.Error {
	failure := agentError(err, operation, target, agentapi.HostUnreachable)
	failure.Message = safeRemoteText(failure.Message)
	return failure
}

func (c *mcpControls) StartService(ctx context.Context, req mcpnative.ServiceRouteRequest) (mcpnative.CallOutcome, error) {
	return c.startStopService(ctx, req, true)
}

func (c *mcpControls) StopService(ctx context.Context, req mcpnative.ServiceRouteRequest) (mcpnative.CallOutcome, error) {
	return c.startStopService(ctx, req, false)
}

func (c *mcpControls) startStopService(ctx context.Context, req mcpnative.ServiceRouteRequest, start bool) (mcpnative.CallOutcome, error) {
	operation, requestType, label := "services.stop", protocol.TypeServiceStop, "service stop"
	if start {
		operation, requestType, label = "services.start", protocol.TypeServiceStart, "service start"
	}
	var privateName string
	return c.serviceCall(ctx, operation, req.Host, req.Timeout, func(ctx context.Context, host HostRecord, exchange serviceExchange) (any, error) {
		name, err := serviceNameFromRoute(req.Name)
		if err != nil {
			return nil, agentapi.Usage(operation, err.Error())
		}
		recording := func(ctx context.Context, request protocol.Control, expected *string) (protocol.Control, protocol.HostInfo, error) {
			response, info, err := exchange(ctx, request, expected)
			privateName = info.PrivateName
			return response, info, err
		}
		status, err := startStopServiceVia(ctx, host, recording, requestType, label, name)
		if err != nil {
			return nil, err
		}
		return c.serviceChange(host, privateName, status, ""), nil
	})
}

func (c *mcpControls) serviceChange(host HostRecord, privateName string, service protocol.ServiceInfo, warning string) agentapi.ServiceChange {
	row := ServiceCatalogRow{Host: host, PrivateName: privateName, Service: service, Live: true}
	change := agentapi.ServiceChange{Service: agentServiceRow(row, c.app.dependencies.Now().UTC()), State: row.State(), Warnings: []string{}}
	if service.Demand != nil && service.Demand.State != protocol.DemandStopped {
		change.SessionID = service.Demand.SessionID
	}
	for line := range strings.SplitSeq(warning, "\n") {
		if line = safeRemoteText(line); line != "" && len(change.Warnings) < maxServiceWarnings {
			change.Warnings = append(change.Warnings, line)
		}
	}
	return change
}

func (c *mcpControls) UnpublishService(ctx context.Context, req mcpnative.ServiceRouteRequest) (mcpnative.CallOutcome, error) {
	return c.serviceCall(ctx, "services.unpublish", req.Host, req.Timeout, func(ctx context.Context, host HostRecord, exchange serviceExchange) (any, error) {
		return unpublishVia(ctx, host, exchange, req.Name)
	})
}

// unpublishVia removes one route. service.not_found means the host's own
// listing proved the route absent; a failed listing keeps its own cause, and
// a retry after an uncertain reply is answered by that listing.
func unpublishVia(ctx context.Context, host HostRecord, exchange serviceExchange, route string) (agentapi.ServiceRemoval, error) {
	const operation = "services.unpublish"
	name, err := serviceNameFromRoute(route)
	if err != nil {
		return agentapi.ServiceRemoval{}, agentapi.Usage(operation, err.Error())
	}
	_, found, err := existingServiceVia(ctx, host, exchange, name)
	if err != nil {
		return agentapi.ServiceRemoval{}, err
	}
	if !found {
		return agentapi.ServiceRemoval{}, agentapi.Fail(agentapi.ServiceNotFound, operation, host.ID, fmt.Sprintf("route %s is not served by %s", safeRemoteText(route), HostLabel(host)))
	}
	if err := deleteServiceVia(ctx, host, exchange, name); err != nil {
		return agentapi.ServiceRemoval{}, err
	}
	return agentapi.ServiceRemoval{HostID: host.ID, Name: name, Removed: true}, nil
}

func (c *mcpControls) PublishService(ctx context.Context, req mcpnative.PublishServiceRequest) (mcpnative.CallOutcome, error) {
	const operation = "services.publish"
	flags, err := publishFlags(req)
	if err != nil {
		return failOutcome(operation, nil, agentapi.Fail(agentapi.UsageInvalid, operation, req.Host, err.Error()).Err), nil
	}
	return c.serviceCall(ctx, operation, req.Host, req.Timeout, func(ctx context.Context, host HostRecord, exchange serviceExchange) (any, error) {
		publication, err := publishVia(ctx, host, exchange, req.Target, flags)
		if err != nil {
			return nil, err
		}
		return c.serviceChange(host, publication.PrivateName, publication.Service, publication.Warning), nil
	})
}

// serviceLookupError marks a failed read of the host's current routes, so it
// is reported as what it is and never as bad input.
type serviceLookupError struct{ err error }

func (e *serviceLookupError) Error() string { return e.err.Error() }
func (e *serviceLookupError) Unwrap() error { return e.err }

func publishVia(ctx context.Context, host HostRecord, exchange serviceExchange, target string, flags serveFlags) (remoteServicePublication, error) {
	existing := func(ctx context.Context, name string) (protocol.ServiceInfo, bool, error) {
		service, found, err := existingServiceVia(ctx, host, exchange, name)
		if err != nil {
			return service, found, &serviceLookupError{err: err}
		}
		return service, found, nil
	}
	requested, err := serviceRequestFromFlags(ctx, host, target, flags, existing)
	var lookup *serviceLookupError
	if errors.As(err, &lookup) {
		return remoteServicePublication{}, lookup.err
	}
	if err != nil {
		return remoteServicePublication{}, agentapi.Usage("services.publish", err.Error())
	}
	return publishServiceVia(ctx, host, exchange, requested)
}

func existingServiceVia(ctx context.Context, host HostRecord, exchange serviceExchange, name string) (protocol.ServiceInfo, bool, error) {
	response, info, err := exchange(ctx, protocol.Control{Type: protocol.TypeServiceList}, nil)
	if err != nil {
		return protocol.ServiceInfo{}, false, err
	}
	snapshot, err := decodeRemoteServices(host, response, info)
	if err != nil {
		return protocol.ServiceInfo{}, false, err
	}
	for _, service := range snapshot.Services {
		if service.Name == name {
			return service, true, nil
		}
	}
	return protocol.ServiceInfo{}, false, nil
}

// publishFlags maps tool options onto mesh serve's flags. A run command
// needs an absolute cwd: the frontend's own directory means nothing to the
// agent that called it.
func publishFlags(req mcpnative.PublishServiceRequest) (serveFlags, error) {
	flags := serveFlags{
		route: req.Route, displayName: req.Label, files: req.Files, isolate: req.Isolate,
		run: req.Run, cwd: req.Cwd, cwdSet: req.Cwd != "", env: req.Env, listens: req.Listens,
		idle: meshserve.DefaultIdle, readyTimeout: meshserve.DefaultReadyTimeout,
		keepRunning: req.KeepRunning, keepSet: req.KeepRunning,
	}
	if req.Run != "" && !filepath.IsAbs(req.Cwd) {
		return flags, errors.New("run needs an absolute cwd on the host")
	}
	if req.IdleMS != nil {
		flags.idle, flags.idleSet = time.Duration(*req.IdleMS)*time.Millisecond, true
	}
	if req.ReadyTimeoutMS != nil {
		flags.readyTimeout, flags.readySet = time.Duration(*req.ReadyTimeoutMS)*time.Millisecond, true
	}
	return flags, nil
}

// ListApps uses the same transport as mesh app list: the owning host's local
// socket, reached over Mesh SSH for another machine. App owner operations
// stay local-only on their host.
func (c *mcpControls) ListApps(ctx context.Context, req mcpnative.ListAppsRequest) (mcpnative.CallOutcome, error) {
	const operation = "apps.list"
	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	hostID, target, err := c.appHost(callCtx, req.Host)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	transport, err := c.app.openApps(callCtx, hostID, bootstrap.DefaultSSHPort)
	if err != nil {
		return failOutcome(operation, target, boundedAgentError(err, operation, target.HostID)), nil
	}
	defer transport.close() //nolint:errcheck // the listing is authoritative
	result, err := transport.request(callCtx, appspkg.Request{Action: appActionList})
	if err != nil {
		return failOutcome(operation, target, boundedAgentError(err, operation, target.HostID)), nil
	}
	return okOutcome(operation, target, agentApps(target.HostID, result.Apps)), nil
}

func (c *mcpControls) appHost(ctx context.Context, value string) (string, *mcpnative.TargetMeta, error) {
	if value == appLocalHost {
		return appLocalHost, &mcpnative.TargetMeta{HostID: appLocalHost, Account: c.account}, nil
	}
	host, err := c.resolveSessionHost(ctx, value)
	if err != nil {
		return "", nil, err
	}
	if isThisHost(host) {
		return appLocalHost, c.targetMeta(host, protocol.HostInfo{}), nil
	}
	return host.ID, c.targetMeta(host, protocol.HostInfo{}), nil
}

func agentApps(hostID string, records []appspkg.Record) agentapi.AppsData {
	data := agentapi.AppsData{HostID: hostID, Apps: make([]agentapi.AppRow, 0, min(len(records), agentapi.MaxAppRows))}
	for _, record := range records {
		if len(data.Apps) == agentapi.MaxAppRows {
			data.Truncated = true
			break
		}
		data.Apps = append(data.Apps, agentapi.AppRow{ID: record.ID, Kind: record.Kind, Status: safeRemoteText(record.Status), Cleanup: safeRemoteText(record.Cleanup), Ready: record.Ready, ExpiresAt: record.ExpiresAt.UTC(), URL: appspkg.URL(record.ID)})
	}
	return data
}

// HibernateSession sends the same control as mesh hibernate. The host
// accepts only a detached agent session, and nothing attaches to it.
func (c *mcpControls) HibernateSession(ctx context.Context, req mcpnative.SessionLifecycleRequest) (mcpnative.CallOutcome, error) {
	const operation = "sessions.hibernate"
	return c.sessionLifecycle(ctx, operation, req, protocol.Control{Type: protocol.TypeHibernate, SessionID: req.SessionID, HumanSessionOnly: true}, func(host HostRecord, response protocol.Control) (any, error) {
		if response.Type != protocol.TypeOK || response.SessionID != req.SessionID {
			return nil, fmt.Errorf("host %s returned an unexpected %s response", HostLabel(host), protocol.TypeHibernate)
		}
		return agentapi.SessionHibernation{HostID: host.ID, SessionID: req.SessionID, Hibernated: true}, nil
	})
}

// RecoverSession sends the same control as mesh recover without the attach
// that follows it there. The host refuses private and keyed application
// workers.
func (c *mcpControls) RecoverSession(ctx context.Context, req mcpnative.SessionLifecycleRequest) (mcpnative.CallOutcome, error) {
	const operation = "sessions.recover"
	control := protocol.Control{Type: protocol.TypeRecover, SessionID: req.SessionID, RecoveryAction: req.Action, Cols: 80, Rows: 24, Term: recoveryTerm, RecoveryRequireInterrupted: true}
	return c.sessionLifecycle(ctx, operation, req, control, func(host HostRecord, response protocol.Control) (any, error) {
		return recoveredReplacement(host, req, response)
	})
}

// recoveredReplacement accepts only a replacement for the requested source.
// The host refuses a live source under its recovery lock; a retry after a
// lost reply returns the recorded replacement, which names this source.
func recoveredReplacement(host HostRecord, req mcpnative.SessionLifecycleRequest, response protocol.Control) (agentapi.SessionRecovery, error) {
	result, err := recoveredResponse(response)
	if err != nil {
		return agentapi.SessionRecovery{}, err
	}
	if result.Remote != nil || result.SessionID == "" || result.SessionID == req.SessionID || result.RecoveredFrom != req.SessionID {
		return agentapi.SessionRecovery{}, fmt.Errorf("host %s returned no local replacement session", HostLabel(host))
	}
	return agentapi.SessionRecovery{HostID: host.ID, SessionID: result.SessionID, RecoveredFrom: result.RecoveredFrom, Action: req.Action, AgentStatus: result.AgentStatus}, nil
}

func (c *mcpControls) sessionLifecycle(ctx context.Context, operation string, req mcpnative.SessionLifecycleRequest, control protocol.Control, build func(HostRecord, protocol.Control) (any, error)) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome(operation, ""), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveSessionHost(callCtx, req.Host)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	response, info, err := c.pool.ExchangeShort(callCtx, host, control, false)
	target := c.targetMeta(host, info)
	if err != nil {
		return failOutcome(operation, target, boundedAgentError(err, operation, host.ID)), nil
	}
	if response.Type == protocol.TypeError {
		code := controlAgentCode(response.ErrorCode)
		if response.ErrorCode == "" {
			code = agentapi.UsageInvalid
		}
		return failOutcome(operation, target, agentapi.Fail(code, operation, host.ID, safeRemoteText(response.Message)).Err), nil
	}
	data, err := build(host, response)
	if err != nil {
		return failOutcome(operation, target, boundedAgentError(err, operation, host.ID)), nil
	}
	return okOutcome(operation, target, data), nil
}
