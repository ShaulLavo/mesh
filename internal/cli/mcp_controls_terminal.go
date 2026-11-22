package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

// terminalCreateTimeout covers a worker launch; the tool's own deadline is
// for the reply, and a launch that is cut short is recovered by its key.
const terminalCreateTimeout = 15 * time.Second

func (c *mcpControls) TerminalOpen(ctx context.Context, req mcpnative.TerminalOpenRequest) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome("sessions.create", req.Context.HostID), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, max(req.Timeout, terminalCreateTimeout))
	defer cancel()
	host, err := c.resolveExactHostID(req.Context.HostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	control := protocol.Control{
		Type:          protocol.TypeExecutionCreate,
		Caller:        c.caller,
		ExecutionKind: execution.KindPTY,
		RequestKey:    req.RequestKey,
		Command:       req.Argv,
		Cwd:           req.Cwd,
		Cols:          req.Cols,
		Rows:          req.Rows,
		Term:          req.Term,
		ExecutionContext: &protocol.ExecutionContextRequest{
			ContextID:    req.Context.ContextID,
			ShellCommand: req.Command,
			Env:          req.Env,
		},
	}
	if err := validateControlBeforeSend(&control); err != nil {
		target := &mcpnative.TargetMeta{HostID: host.ID, HostName: HostLabel(host), ContextID: req.Context.String()}
		return failOutcome("sessions.create", target, agentapi.Usage("sessions.create", err.Error()).Err), nil
	}
	response, info, err := c.pool.ExchangeShort(callCtx, host, control, false)
	target := c.targetMeta(host, info)
	target.ContextID = req.Context.String()
	if err != nil {
		return c.execCreateTransportFailure(err, host.ID, req.RequestKey, target), nil
	}
	receipt, failure, ok := c.decodeExecReceipt("sessions.create", response, host.ID, req.RequestKey, target)
	if !ok {
		return failure, nil
	}
	ref := mcpnative.JobRef{HostID: receipt.HostID, SessionID: receipt.SessionID, Instance: receipt.Instance}
	return okOutcome("sessions.create", target, mcpnative.JobStateData{
		Job:        ref.String(),
		RequestKey: receipt.RequestKey,
		Target:     *target,
		State:      mcpnative.JobStateFromReceipt(receipt),
		Result:     receipt.Result,
		Receipt:    receipt,
	}), nil
}

func (c *mcpControls) TerminalResize(ctx context.Context, req mcpnative.TerminalResizeRequest) (mcpnative.CallOutcome, error) {
	control := protocol.Control{Type: protocol.TypeExecutionResize, Cols: req.Cols, Rows: req.Rows}
	return c.runTerminalControl(ctx, agentSessionResize, req.Job, req.Context, req.Timeout, control)
}

func (c *mcpControls) TerminalScreen(ctx context.Context, req mcpnative.TerminalScreenRequest) (mcpnative.CallOutcome, error) {
	control := protocol.Control{Type: protocol.TypeExecutionScreen, PreviewCols: req.Cols, PreviewRows: req.Rows}
	return c.runTerminalControl(ctx, agentSessionScreen, req.Job, req.Context, req.Timeout, control)
}

// terminalToolData wraps the shared CLI result with the job handle.
func terminalToolData(response, control protocol.Control, job mcpnative.JobRef, target *mcpnative.TargetMeta) (any, error) {
	if control.Type == protocol.TypeExecutionResize {
		size, err := agentTerminalSize(response, control, job.HostID)
		return mcpnative.TerminalSizeData{Job: job.String(), Target: *target, Size: size}, err
	}
	screen, err := agentTerminalScreen(response, control, job.HostID)
	return mcpnative.TerminalScreenData{Job: job.String(), Target: *target, Screen: screen}, err
}

func (c *mcpControls) runTerminalControl(ctx context.Context, operation string, job mcpnative.JobRef, ctxRef *mcpnative.ContextRef, timeout time.Duration, control protocol.Control) (mcpnative.CallOutcome, error) {
	// Without an identity the request would carry whole-account authority;
	// an agent reaches only terminal jobs its own identity created.
	if c.caller == "" {
		return c.identityRequiredOutcome(operation, job.HostID), nil
	}
	control.SessionID, control.Instance = job.SessionID, job.Instance
	c.attachCaller(&control, ctxRef)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, err := c.resolveExactHostID(job.HostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	response, info, err := c.pool.ExchangeShort(callCtx, host, control, control.Type == protocol.TypeExecutionScreen)
	target := c.targetMeta(host, info)
	if ctxRef != nil {
		target.ContextID = ctxRef.String()
	}
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, host.ID, agentapi.HostUnreachable)), nil
	}
	if response.Type == protocol.TypeError {
		return failOutcome(operation, target, agentapi.Fail(controlAgentCode(response.ErrorCode), operation, host.ID, safeRemoteText(response.Message)).Err), nil
	}
	data, err := terminalToolData(response, control, job, target)
	if err != nil {
		return failOutcome(operation, target, agentapi.Fail(agentapi.InternalError, operation, host.ID, err.Error()).Err), nil
	}
	return okOutcome(operation, target, data), nil
}

const executionPhaseTerminal = "terminal"

const (
	agentSessionResize = "sessions.resize"
	agentSessionScreen = "sessions.screen"
)

// agentTerminalSize and agentTerminalScreen build the results both mesh
// session and mesh mcp return, from one exact-instance reply.
func agentTerminalSize(response, request protocol.Control, host string) (agentapi.TerminalSize, error) {
	if err := sameExecutionInstance(response, request); err != nil {
		return agentapi.TerminalSize{}, err
	}
	if response.Cols != request.Cols || response.Rows != request.Rows {
		return agentapi.TerminalSize{}, fmt.Errorf("host reported size %dx%d, not %dx%d", response.Cols, response.Rows, request.Cols, request.Rows)
	}
	return agentapi.TerminalSize{HostID: host, SessionID: request.SessionID, Instance: request.Instance, Cols: response.Cols, Rows: response.Rows}, nil
}

func agentTerminalScreen(response, request protocol.Control, host string) (agentapi.TerminalScreen, error) {
	if err := sameExecutionInstance(response, request); err != nil {
		return agentapi.TerminalScreen{}, err
	}
	inspection := response.Inspection
	if inspection == nil {
		return agentapi.TerminalScreen{}, fmt.Errorf("host returned no screen")
	}
	lines := inspection.Preview
	if lines == nil {
		lines = []string{}
	}
	screen := agentapi.TerminalScreen{HostID: host, SessionID: request.SessionID, Instance: request.Instance, Attached: inspection.Attached, Title: safeRemoteText(inspection.TerminalTitle), Lines: lines, Cols: request.PreviewCols, Rows: request.PreviewRows}
	if screen.Cols == 0 {
		screen.Cols, screen.Rows = 80, 24
	}
	if !inspection.ObservedAt.IsZero() {
		at := inspection.ObservedAt.UTC()
		screen.ObservedAt = &at
	}
	screen.Live = response.ExecutionReceipt != nil && response.ExecutionReceipt.Phase != executionPhaseTerminal
	return screen, nil
}

func sameExecutionInstance(response, request protocol.Control) error {
	receipt := response.ExecutionReceipt
	if receipt != nil && (receipt.SessionID != request.SessionID || receipt.Instance != request.Instance) {
		return fmt.Errorf("host returned another execution instance")
	}
	return nil
}
