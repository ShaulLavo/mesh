package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

const (
	agentReachable          = "reachable"
	agentUnavailable        = "unavailable"
	agentRunning            = "running"
	agentUnknown            = "unknown"
	agentInspectCommandName = "inspect"
)

type agentHostRead struct {
	host       agentapi.HostSummary
	liveAccess bool
	useful     bool
	errors     []agentapi.Error
}

type agentHostReader struct {
	state     string
	config    string
	caller    string
	operation string
	full      bool
	cached    bool
	dial      HostDialer
	now       func() time.Time
}

func (r agentHostReader) read(ctx context.Context, host HostRecord) agentHostRead {
	if host.local {
		return r.local(ctx, host)
	}
	cache := hostprofile.OpenContextCache(r.state)
	entry, cacheErr := cache.Read(host.ID, host.MeshIdentity)
	result := r.cachedResult(host, entry)
	if cacheErr != nil {
		result.errors = append(result.errors, r.diagnostic(cacheErr, host.ID, agentapi.StateUnavailable))
	}
	if r.cached {
		return r.finish(result)
	}
	reply, info, err := readAgentHostContext(ctx, host, r.full, r.dial)
	if err != nil {
		result.host.Reachability = "unreachable"
		result.errors = append(result.errors, r.diagnostic(err, host.ID, agentapi.HostUnreachable))
		return r.finish(result)
	}
	at := r.now().UTC()
	observation := hostObservation(info)
	result.host.MachineName, result.host.NameShadowed = info.MachineName, machinename.ShadowedByCommand(info.MachineName)
	result.host.Reachability, result.host.Observed = agentReachable, agentObservation(&observation, at, agentapi.FreshnessLive)
	result.host.Declared = contextDeclared(reply, agentapi.FreshnessLive, at)
	result.useful, result.liveAccess = true, true
	result.errors = append(result.errors, r.contextProblems(reply, host.ID)...)
	r.cacheLive(ctx, host, hostprofile.CachedContext{HostID: host.ID, MeshIdentity: host.MeshIdentity, ReceivedAt: at, Observed: &observation, Context: reply}, &result)
	return r.finish(result)
}

func readAgentHostContext(ctx context.Context, host HostRecord, full bool, dial HostDialer) (hostprofile.ContextReply, protocol.HostInfo, error) {
	conn, info, err := openAgentHostInfo(ctx, host, dial)
	if err != nil {
		return hostprofile.ContextReply{}, info, err
	}
	defer func() { _ = conn.Close() }()
	if err := validateContextObservation(info); err != nil {
		return hostprofile.ContextReply{}, info, err
	}
	data, err := json.Marshal(hostprofile.ContextRequest{Full: full})
	if err != nil {
		return hostprofile.ContextReply{}, info, fmt.Errorf("encode host context request: %w", err)
	}
	id, err := newDaemonRequestID()
	if err != nil {
		return hostprofile.ContextReply{}, info, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeHostContext, RequestID: id, Context: data})
	if err != nil {
		return hostprofile.ContextReply{}, info, err
	}
	if response.Type == protocol.TypeError {
		return hostprofile.ContextReply{}, info, daemonResponseError("host context", response.Message, response.ErrorCode)
	}
	if response.Type != protocol.TypeHostContextResult {
		return hostprofile.ContextReply{}, info, errors.New("host did not return host context")
	}
	reply, err := hostprofile.ParseContext(response.Context, host.ID)
	if err != nil {
		return hostprofile.ContextReply{}, info, fmt.Errorf("validate pinned host context: %w", err)
	}
	if full && (reply.Host.Completeness != hostprofile.Full || reply.Fleet.Completeness != hostprofile.Full) {
		return hostprofile.ContextReply{}, info, errors.New("host returned incomplete context for inspect")
	}
	return reply, info, nil
}

func (r agentHostReader) cacheLive(ctx context.Context, host HostRecord, entry hostprofile.CachedContext, result *agentHostRead) {
	if _, _, err := identity.LoadPrivate(r.state); err != nil {
		result.errors = appendHostDiagnostic(result.errors, r.diagnostic(err, host.ID, agentapi.StateUnavailable))
		return
	}
	regressed, err := hostprofile.OpenContextCache(r.state).Save(ctx, entry)
	if err != nil {
		result.errors = appendHostDiagnostic(result.errors, r.diagnostic(err, host.ID, agentapi.StateUnavailable))
	}
	if regressed {
		result.errors = append(result.errors, r.diagnostic(errors.New("The live profile revision is lower than the cached revision; live data is shown."), host.ID, agentapi.ProfileRegressed))
	}
}

func (r agentHostReader) cachedResult(host HostRecord, entry *hostprofile.CachedContext) agentHostRead {
	result := agentHostRead{host: agentHostBase(host)}
	if entry == nil {
		return result
	}
	result.useful = true
	result.host.Observed = agentObservation(entry.Observed, entry.ReceivedAt, agentapi.FreshnessCached)
	result.host.Declared = contextDeclared(entry.Context, agentapi.FreshnessCached, entry.ReceivedAt)
	return result
}

func (r agentHostReader) finish(result agentHostRead) agentHostRead {
	if r.full {
		result.host.Declared.Host = r.completeCachedSection(result.host.Declared.Host, result.host.HostID, &result.errors)
		result.host.Declared.Fleet.ProfileSection = r.completeCachedSection(result.host.Declared.Fleet.ProfileSection, result.host.HostID, &result.errors)
	} else {
		result.host.Declared.Host = summarizeAgentProfile(result.host.Declared.Host, false)
		result.host.Declared.Fleet.ProfileSection = summarizeAgentProfile(result.host.Declared.Fleet.ProfileSection, true)
	}
	return result
}

func (r agentHostReader) completeCachedSection(section agentapi.ProfileSection, id *string, diagnostics *[]agentapi.Error) agentapi.ProfileSection {
	if section.Completeness == agentapi.CompletenessFull || section.State == "absent" {
		return section
	}
	target := ""
	if id != nil {
		target = *id
	}
	message := "Full profile content is not cached. Run mesh inspect HOST_ID --json while the machine is reachable."
	section.State, section.Problem, section.Profile = agentUnavailable, message, nil
	*diagnostics = appendHostDiagnostic(*diagnostics, r.diagnostic(errors.New(message), target, agentapi.StateUnavailable))
	return section
}

func (r agentHostReader) local(ctx context.Context, host HostRecord) agentHostRead {
	result := agentHostRead{host: agentHostBase(host), useful: true}
	if host.ID == "" {
		result.host.Reachability = "uninitialized"
		result.errors = append(result.errors, r.diagnostic(errors.New("This machine has no Mesh identity yet. Set up Mesh here first with mesh install."), "", agentapi.HostUninitialized))
		return result
	}
	authority := &update.Authority{StateDir: r.state, ID: host.ID}
	reply := hostprofile.ReadContext(r.config, r.state, host.ID, authority.IsAdministrator, r.full)
	result.host.Declared = contextDeclared(reply, agentapi.FreshnessLive, r.now().UTC())
	result.errors = r.contextProblems(reply, host.ID)
	if r.cached {
		return result
	}
	probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	conn, info, err := openAgentHostInfo(probe, host, r.dial)
	if err != nil {
		at := r.now().UTC()
		daemon := agentUnknown
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			daemon = "not-running"
		}
		result.host.Observed = &agentapi.Observed{Platform: release.CurrentPlatform().String(), Daemon: daemon, ObservedAt: at, Freshness: agentapi.FreshnessLive}
		result.host.Reachability = agentReachable
		if daemon == agentUnknown {
			result.errors = append(result.errors, r.diagnostic(err, host.ID, agentapi.HostUnreachable))
		}
		return result
	}
	_ = conn.Close()
	if err := validateContextObservation(info); err != nil {
		result.errors = append(result.errors, r.diagnostic(err, host.ID, agentapi.HostUnreachable))
		return result
	}
	observation := hostObservation(info)
	result.host.Observed = agentObservation(&observation, r.now().UTC(), agentapi.FreshnessLive)
	result.host.Reachability, result.liveAccess = agentReachable, true
	return result
}

func (r agentHostReader) contextProblems(reply hostprofile.ContextReply, id string) []agentapi.Error {
	diagnostics := []agentapi.Error{}
	for _, section := range []hostprofile.WireSection{reply.Host, reply.Fleet} {
		if section.State == hostprofile.StateUnavailable {
			diagnostics = append(diagnostics, r.diagnostic(errors.New(section.Problem), id, agentapi.StateUnavailable))
		}
	}
	return diagnostics
}

func (r agentHostReader) diagnostic(err error, target string, fallback agentapi.Code) agentapi.Error {
	return agentError(err, r.operation, target, fallback)
}

func agentHostBase(host HostRecord) agentapi.HostSummary {
	var id *string
	if host.ID != "" {
		id = &host.ID
	}
	unavailable := agentapi.ProfileSection{State: agentUnavailable, Completeness: agentapi.CompletenessSummary, Freshness: agentapi.FreshnessCached, Problem: "No context observation is cached."}
	return agentapi.HostSummary{HostID: id, MachineName: host.MachineName, Local: host.local, NameShadowed: machinename.ShadowedByCommand(host.MachineName), Reachability: "not-queried", Declared: agentapi.Declared{Host: unavailable, Fleet: agentapi.FleetSection{ProfileSection: unavailable}}}
}

func contextDeclared(reply hostprofile.ContextReply, freshness agentapi.Freshness, received time.Time) agentapi.Declared {
	return agentapi.Declared{Host: agentContextSection(reply.Host, freshness, received), Fleet: agentapi.FleetSection{ProfileSection: agentContextSection(reply.Fleet, freshness, received), Fleet: reply.Fleet.Fleet, Author: reply.Fleet.Author}}
}

func agentContextSection(section hostprofile.WireSection, freshness agentapi.Freshness, received time.Time) agentapi.ProfileSection {
	result := agentapi.ProfileSection{State: agentapi.ProfileState(section.State), Completeness: agentapi.Completeness(section.Completeness), Problem: section.Problem, Revision: section.Revision, Digest: section.Digest, UpdatedAt: section.UpdatedAt, Freshness: freshness, ReceivedAt: &received}
	if section.Profile == nil {
		return result
	}
	var instructions *string
	if section.Completeness == hostprofile.Full {
		instructions = &section.Profile.Instructions
	}
	labels, warnings := section.Profile.Labels, section.Profile.Warnings
	if labels == nil {
		labels = map[string]string{}
	}
	if warnings == nil {
		warnings = []string{}
	}
	result.Profile = &agentapi.Profile{Description: section.Profile.Description, Labels: labels, Warnings: warnings, Instructions: instructions, InstructionsBytes: section.InstructionsBytes}
	return result
}

func summarizeAgentProfile(section agentapi.ProfileSection, fleet bool) agentapi.ProfileSection {
	section.Completeness = agentapi.CompletenessSummary
	if fleet {
		section.Profile = nil
		return section
	}
	if section.Profile != nil {
		p := *section.Profile
		p.Instructions = nil
		section.Profile = &p
	}
	return section
}

func hostObservation(info protocol.HostInfo) hostprofile.ContextObservation {
	result := hostprofile.ContextObservation{Daemon: agentRunning}
	if info.Build != nil {
		result.Version, result.Platform = info.Build.Version, info.Build.Platform.String()
	}
	return result
}

func agentObservation(observed *hostprofile.ContextObservation, at time.Time, freshness agentapi.Freshness) *agentapi.Observed {
	if observed == nil {
		return nil
	}
	return &agentapi.Observed{Version: observed.Version, Platform: observed.Platform, Daemon: observed.Daemon, ObservedAt: at, Freshness: freshness}
}

func (r agentHostReader) access(result agentHostRead, host HostRecord) agentapi.Access {
	access := agentapi.Access{State: agentUnavailable, Transport: "pinned-tls", Grant: agentUnknown, Enforcement: "device-grant", Reason: "No current authenticated context observation is available.", Summary: "Current access was not observed. Mesh has no per-operation agent scoping."}
	if host.local {
		access.Transport = "local-socket"
	}
	if r.caller != "" {
		access.Caller = &r.caller
	}
	if !result.liveAccess {
		return access
	}
	access.State, access.Reason = "live", ""
	access.ObservedAt = &result.host.Observed.ObservedAt
	access.Grant = "full-device"
	access.Summary = "This device has full control of this machine. Mesh does not restrict what an agent does with that access."
	if host.local {
		access.Grant = "same-account"
		access.Summary = "The local socket runs as this account. Mesh does not restrict what an agent does with that access."
	}
	return access
}

func newAgentHostReader(operation string, full, cached bool, a *application) (agentHostReader, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return agentHostReader{}, fmt.Errorf("locate host context state: %w", err)
	}
	config, err := ConfigPath()
	if err != nil {
		return agentHostReader{}, err
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return agentHostReader{}, err
	}
	return agentHostReader{state: state, config: filepath.Dir(config), caller: local.ID, operation: operation, full: full, cached: cached, dial: a.dependencies.DialPassive, now: a.dependencies.Now}, nil
}

func appendHostDiagnostic(diagnostics []agentapi.Error, next agentapi.Error) []agentapi.Error {
	for _, existing := range diagnostics {
		if existing == next {
			return diagnostics
		}
	}
	return append(diagnostics, next)
}

func validateContextObservation(info protocol.HostInfo) error {
	observation := hostObservation(info)
	for _, field := range []struct {
		value string
		limit int
	}{{observation.Version, 256}, {observation.Platform, 64}} {
		if len(field.value) > field.limit || agentapi.CleanMessage(field.value) != field.value {
			return errors.New("Host build observation contains invalid text or exceeds its byte limit.")
		}
	}
	return nil
}
