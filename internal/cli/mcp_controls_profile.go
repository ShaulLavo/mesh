package cli

import (
	"context"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/paths"
)

// The profile and fleet tools act on this machine's own state, the machine
// that runs mesh mcp, and never contact a peer. They have no host argument.

// localToolTarget names this machine in a result.
func (c *mcpControls) localToolTarget() *mcpnative.TargetMeta {
	state, err := paths.StateDirPath()
	if err != nil {
		return nil
	}
	host, err := identity.Load(state)
	if err != nil {
		return &mcpnative.TargetMeta{Account: c.account}
	}
	meta := &mcpnative.TargetMeta{HostID: host.ID, Account: c.account}
	if local, err := agentLocalHost(state); err == nil {
		meta.HostName = local.MachineName
	}
	return meta
}

func (c *mcpControls) FleetStatus(ctx context.Context, _ mcpnative.FleetStatusRequest) (mcpnative.CallOutcome, error) {
	const operation = "profile.status"
	target := c.localToolTarget()
	local, err := openLocalFleetProfile()
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, "fleet", agentapi.StateUnavailable)), nil
	}
	snapshot, outbox, readErr := local.store.Read(local.scope.Trusted)
	data := agentapi.FleetStatus{Replica: fleetProfileSection(snapshot), FleetSaved: local.saved, Fleet: local.name, Members: fleetDeliveries(outbox.Members)}
	if readErr != nil {
		problem := agentError(readErr, operation, "fleet/outbox.json", agentapi.StateUnavailable)
		return mcpnative.CallOutcome{Envelope: agentapi.Envelope{Operation: operation, Status: agentapi.StatusPartial, Data: data, Errors: []agentapi.Error{problem}}, Target: target}, nil
	}
	return okOutcome(operation, target, data), nil
}

func (c *mcpControls) ProfileGet(ctx context.Context, req mcpnative.ProfileGetRequest) (mcpnative.CallOutcome, error) {
	const operation = "profile.show"
	target := c.localToolTarget()
	if req.Scope == mcpnative.ProfileScopeFleet {
		local, err := openLocalFleetProfile()
		if err != nil {
			return failOutcome(operation, target, agentError(err, operation, "fleet", agentapi.StateUnavailable)), nil
		}
		snapshot, err := local.store.ReadReplica(local.scope.Trusted)
		if err != nil {
			return failOutcome(operation, target, agentError(err, operation, "fleet", agentapi.StateUnavailable)), nil
		}
		return okOutcome(operation, target, fleetProfileSection(snapshot)), nil
	}
	store, _, err := localProfileStore()
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, "profile.json", agentapi.StateUnavailable)), nil
	}
	snapshot, err := store.Read()
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, "profile.json", agentapi.StateUnavailable)), nil
	}
	section, err := profileSection(snapshot)
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, "profile.json", agentapi.StateUnavailable)), nil
	}
	return okOutcome(operation, target, section), nil
}

func (c *mcpControls) ProfileSet(ctx context.Context, req mcpnative.ProfileSetRequest) (mcpnative.CallOutcome, error) {
	const operation = "profile.set"
	target := c.localToolTarget()
	if req.Scope == mcpnative.ProfileScopeFleet {
		return c.setFleetNotes(ctx, operation, target, req), nil
	}
	store, _, err := localProfileStore()
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, "profile.json", agentapi.StateUnavailable)), nil
	}
	next, err := hostprofile.ParseEditable(req.Document, hostprofile.HostLimits)
	if err != nil {
		return failOutcome(operation, target, agentError(profileFailure(err), operation, "profile.json", agentapi.ProfileInvalid)), nil
	}
	written, changed, err := store.Set(ctx, req.ExpectedRevision, next)
	if err != nil {
		return failOutcome(operation, target, agentError(profileFailure(err), operation, "profile.json", agentapi.StateUnavailable)), nil
	}
	return okOutcome(operation, target, hostProfileWrite(written, changed)), nil
}

// setFleetNotes signs and publishes the fleet notes from this machine, which
// must be allowed to by the fleet's trust rules, at the expected revision.
func (c *mcpControls) setFleetNotes(ctx context.Context, operation string, target *mcpnative.TargetMeta, req mcpnative.ProfileSetRequest) mcpnative.CallOutcome {
	local, err := openLocalFleetProfile()
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, "fleet", agentapi.StateUnavailable))
	}
	next, err := hostprofile.ParseEditable(req.Document, hostprofile.FleetLimits)
	if err != nil {
		return failOutcome(operation, target, agentError(profileFailure(wrapProfileError(err)), operation, "fleet/profile.json", agentapi.ProfileInvalid))
	}
	result, err := local.write(ctx, req.ExpectedRevision, next)
	if err == nil {
		pokeFleetProfile(ctx, local.state)
	}
	data := fleetWriteResult(result, local.saved)
	if err == nil {
		return okOutcome(operation, target, data)
	}
	if result.Publication != hostprofile.ReplicaDurable {
		return failOutcome(operation, target, agentError(profileFailure(err), operation, "fleet/profile.json", agentapi.StateUnavailable))
	}
	problem := agentError(err, operation, "fleet/profile.json", agentapi.StateUnavailable)
	return mcpnative.CallOutcome{Envelope: agentapi.Envelope{Operation: operation, Status: agentapi.StatusPartial, Data: data, Errors: []agentapi.Error{problem}}, Target: target}
}
