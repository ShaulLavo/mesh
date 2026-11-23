package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// runReplyGrace is the run's client deadline on top of its own: the host's
// hang-up grace and output drain after the deadline, plus the round trip.
const runReplyGrace = 5 * time.Second

// Run serves mesh_run: one execution.run on the host's pooled connection and
// its single reply. Nothing is retried and nothing is kept.
func (c *mcpControls) Run(ctx context.Context, req mcpnative.RunRequest) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome(mcpnative.RunOperation, req.Context.HostID), nil
	}
	host, err := c.resolveExactHostID(req.Context.HostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	control := mcpnative.RunControl(req)
	control.Caller = c.caller
	target := &mcpnative.TargetMeta{HostID: host.ID, HostName: HostLabel(host), ContextID: req.Context.String()}
	if err := validateControlBeforeSend(&control); err != nil {
		return failOutcome(mcpnative.RunOperation, target, agentapi.Usage(mcpnative.RunOperation, err.Error()).Err), nil
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond+runReplyGrace)
	defer cancel()
	resp, info, err := c.pool.ExchangeRun(runCtx, host, control)
	if info.ID != "" {
		target = c.targetMeta(host, info)
		target.ContextID = req.Context.String()
	}
	if err != nil {
		return runTransportFailure(err, host.ID, target), nil
	}
	if resp.Type == protocol.TypeError {
		failure := agentapi.Fail(controlAgentCode(resp.ErrorCode), mcpnative.RunOperation, host.ID, resp.Message).Err
		return failOutcome(mcpnative.RunOperation, target, failure), nil
	}
	target.Cwd = resp.ExecutionRun.Cwd
	return okOutcome(mcpnative.RunOperation, target, mcpnative.BuildRunResult(resp.ExecutionRun, *target)), nil
}

// runTransportFailure says nothing ran only when the request provably never
// left; every later failure leaves the outcome unknown.
func runTransportFailure(err error, hostID string, target *mcpnative.TargetMeta) mcpnative.CallOutcome {
	if errors.Is(err, errRunPoolFull) {
		return failOutcome(mcpnative.RunOperation, target, agentapi.Fail(agentapi.ResourceLimit, mcpnative.RunOperation, hostID, "Every mesh_run slot for this host is busy; nothing ran.").Err)
	}
	if isPreDispatchError(err) || errors.Is(err, transport.ErrDestinationIdentityMismatch) || errors.Is(err, transport.ErrAuthentication) {
		return failOutcome(mcpnative.RunOperation, target, agentError(err, mcpnative.RunOperation, hostID, agentapi.HostUnreachable))
	}
	msg := fmt.Sprintf("The command may or may not have run (%v).", err)
	return failOutcome(mcpnative.RunOperation, target, agentapi.Fail(agentapi.RequestOutcomeUnknown, mcpnative.RunOperation, hostID, msg).Err)
}
