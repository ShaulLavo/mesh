package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/transport"
)

const identityRequiredMessage = "This tool requires an application identity. Start mesh mcp --identity NAME after creating one with mesh mcp identity create NAME."

func (c *mcpControls) attachCaller(control *protocol.Control, ctxRef *mcpnative.ContextRef) {
	if c.caller != "" {
		control.Caller = c.caller
	}
	if ctxRef != nil {
		if control.ExecutionContext == nil {
			control.ExecutionContext = &protocol.ExecutionContextRequest{}
		}
		control.ExecutionContext.ContextID = ctxRef.ContextID
	}
}

func (c *mcpControls) identityRequiredOutcome(operation, hostID string) mcpnative.CallOutcome {
	var target *mcpnative.TargetMeta
	if hostID != "" {
		target = &mcpnative.TargetMeta{HostID: hostID}
	}
	return failOutcome(operation, target, agentapi.Usage(operation, identityRequiredMessage).Err)
}

func (c *mcpControls) rememberContextCwd(info protocol.ExecutionContextInfo) {
	if info.Handle == "" || info.Cwd == "" {
		return
	}
	c.cwdMu.Lock()
	c.contextCwds[info.Handle] = info.Cwd
	c.cwdMu.Unlock()
}

func (c *mcpControls) resolvedExecCwd(handle, reqCwd string) string {
	if reqCwd != "" && filepath.IsAbs(reqCwd) {
		return filepath.Clean(reqCwd)
	}
	c.cwdMu.RLock()
	base := c.contextCwds[handle]
	c.cwdMu.RUnlock()
	if base == "" {
		return reqCwd
	}
	if reqCwd == "" {
		return base
	}
	return filepath.Join(base, reqCwd)
}

func (c *mcpControls) OpenContext(ctx context.Context, req mcpnative.OpenContextRequest) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome("contexts.open", ""), nil
	}
	spec := &protocol.ContextSpec{Cwd: req.Cwd, Shell: req.Shell, Env: req.Env, Label: req.Label}
	control := protocol.Control{
		Type:             protocol.TypeContextOpen,
		RequestKey:       req.RequestKey,
		Caller:           c.caller,
		ExecutionContext: &protocol.ExecutionContextRequest{Open: spec},
	}
	return c.runContextByHost(ctx, "contexts.open", req.Host, req.Timeout, control, false)
}

func (c *mcpControls) GetContext(ctx context.Context, req mcpnative.ContextRefRequest) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome("contexts.get", req.Context.HostID), nil
	}
	control := protocol.Control{
		Type:             protocol.TypeContextGet,
		Caller:           c.caller,
		ExecutionContext: &protocol.ExecutionContextRequest{ContextID: req.Context.ContextID},
	}
	return c.runContextByRef(ctx, "contexts.get", req.Context, req.Timeout, control, true)
}

func (c *mcpControls) ListContexts(ctx context.Context, req mcpnative.ListContextsRequest) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome("contexts.list", ""), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveSessionHost(callCtx, req.Host)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	return c.listAllMCPContexts(callCtx, host)
}

func (c *mcpControls) listAllMCPContexts(ctx context.Context, host HostRecord) (mcpnative.CallOutcome, error) {
	control := protocol.Control{
		Type:             protocol.TypeContextList,
		Caller:           c.caller,
		ExecutionContext: &protocol.ExecutionContextRequest{},
	}
	merged := protocol.Control{Type: protocol.TypeContextResult, ContextInfos: []protocol.ExecutionContextInfo{}}
	var lastInfo protocol.HostInfo
	for range maxContextListPages {
		response, info, err := c.pool.ExchangeShort(ctx, host, control, true)
		lastInfo = info
		target := c.targetMeta(host, info)
		if err != nil {
			return failOutcome("contexts.list", target, agentError(err, "contexts.list", host.ID, agentapi.HostUnreachable)), nil
		}
		if response.Type != protocol.TypeContextResult {
			return c.contextOutcome("contexts.list", control, response, host.ID, target), nil
		}
		merged.ContextInfos = append(merged.ContextInfos, response.ContextInfos...)
		if response.ContextNextCursor == "" {
			return c.contextOutcome("contexts.list", control, merged, host.ID, target), nil
		}
		control.ExecutionContext.Cursor = response.ContextNextCursor
	}
	target := c.targetMeta(host, lastInfo)
	return failOutcome("contexts.list", target, agentapi.Fail(agentapi.InternalError, "contexts.list", host.ID, "host returned more context pages than it can hold").Err), nil
}

func (c *mcpControls) ContextReceipt(ctx context.Context, req mcpnative.ContextReceiptRequest) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome("contexts.receipt", ""), nil
	}
	control := protocol.Control{
		Type:       protocol.TypeContextReceipt,
		RequestKey: req.RequestKey,
		Caller:     c.caller,
	}
	return c.runContextByHost(ctx, "contexts.receipt", req.Host, req.Timeout, control, true)
}

func (c *mcpControls) UpdateContext(ctx context.Context, req mcpnative.UpdateContextRequest) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome("contexts.update", req.Context.HostID), nil
	}
	rev := req.ExpectedRevision
	control := protocol.Control{
		Type:   protocol.TypeContextUpdate,
		Caller: c.caller,
		ExecutionContext: &protocol.ExecutionContextRequest{
			ContextID:        req.Context.ContextID,
			ExpectedRevision: &rev,
			Change:           &protocol.ContextChange{Cwd: req.Cwd, SetEnv: req.SetEnv, UnsetEnv: req.UnsetEnv, Label: req.Label},
		},
	}
	return c.runContextByRef(ctx, "contexts.update", req.Context, req.Timeout, control, false)
}

func (c *mcpControls) CloseContext(ctx context.Context, req mcpnative.CloseContextRequest) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome("contexts.close", req.Context.HostID), nil
	}
	control := protocol.Control{
		Type:   protocol.TypeContextClose,
		Caller: c.caller,
		ExecutionContext: &protocol.ExecutionContextRequest{
			ContextID:        req.Context.ContextID,
			ExpectedRevision: req.ExpectedRevision,
		},
	}
	return c.runContextByRef(ctx, "contexts.close", req.Context, req.Timeout, control, false)
}

func (c *mcpControls) runContextByHost(ctx context.Context, operation, hostArg string, timeout time.Duration, control protocol.Control, retryRead bool) (mcpnative.CallOutcome, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, err := c.resolveSessionHost(callCtx, hostArg)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	return c.exchangeContext(callCtx, operation, host, "", control, retryRead)
}

func (c *mcpControls) runContextByRef(ctx context.Context, operation string, ref mcpnative.ContextRef, timeout time.Duration, control protocol.Control, retryRead bool) (mcpnative.CallOutcome, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, err := c.resolveExactHostID(ref.HostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	return c.exchangeContext(callCtx, operation, host, ref.String(), control, retryRead)
}

func (c *mcpControls) exchangeContext(ctx context.Context, operation string, host HostRecord, handle string, control protocol.Control, retryRead bool) (mcpnative.CallOutcome, error) {
	response, info, err := c.pool.ExchangeShort(ctx, host, control, retryRead)
	target := c.targetMeta(host, info)
	target.ContextID = handle
	if err != nil {
		return failOutcome(operation, target, agentError(err, operation, host.ID, agentapi.HostUnreachable)), nil
	}
	return c.contextOutcome(operation, control, response, host.ID, target), nil
}

func (c *mcpControls) contextOutcome(operation string, request, response protocol.Control, hostID string, target *mcpnative.TargetMeta) mcpnative.CallOutcome {
	var data any
	if response.ContextInfo != nil {
		c.rememberContextCwd(*response.ContextInfo)
		item := agentContext(*response.ContextInfo)
		target.ContextID = item.Handle
		target.Cwd = item.Cwd
		data = item
	}
	if request.Type == protocol.TypeContextList && response.Type == protocol.TypeContextResult {
		list := agentapi.ExecutionContexts{HostID: hostID, Contexts: make([]agentapi.ExecutionContext, 0, len(response.ContextInfos))}
		for _, info := range response.ContextInfos {
			c.rememberContextCwd(info)
			list.Contexts = append(list.Contexts, agentContext(info))
		}
		data = list
	}
	if response.Type == protocol.TypeError {
		failure := agentapi.Fail(controlAgentCode(response.ErrorCode), operation, hostID, response.Message).Err
		envelope := agentapi.Envelope{Operation: operation, Status: agentapi.StatusError, Errors: []agentapi.Error{failure}}
		if data != nil {
			envelope.Data = data
		}
		return mcpnative.CallOutcome{Envelope: envelope, Target: target, IsError: true}
	}
	if response.Type != protocol.TypeContextResult || data == nil {
		return failOutcome(operation, target, agentapi.Fail(agentapi.InternalError, operation, hostID, "host did not return a context result").Err)
	}
	return okOutcome(operation, target, data)
}

func (c *mcpControls) Exec(ctx context.Context, req mcpnative.ExecRequest, notify func(mcpnative.ProgressUpdate)) (mcpnative.CallOutcome, error) {
	if c.caller == "" {
		return c.identityRequiredOutcome("sessions.create", req.Context.HostID), nil
	}
	createCtx, cancelCreate := context.WithTimeout(ctx, 15*time.Second)
	defer cancelCreate()
	host, err := c.resolveExactHostID(req.Context.HostID)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	createCtrl := protocol.Control{
		Type:          protocol.TypeExecutionCreate,
		Caller:        c.caller,
		ExecutionKind: execution.KindPipe,
		RequestKey:    req.RequestKey,
		Command:       req.Argv,
		Cwd:           req.Cwd,
		Stdin:         req.Stdin,
		ExecutionContext: &protocol.ExecutionContextRequest{
			ContextID:    req.Context.ContextID,
			ShellCommand: req.Command,
			Env:          req.Env,
		},
	}
	resp, info, err := c.pool.ExchangeShort(createCtx, host, createCtrl, false)
	target := c.targetMeta(host, info)
	target.ContextID = req.Context.String()
	target.Cwd = c.resolvedExecCwd(req.Context.String(), req.Cwd)
	if err != nil {
		return c.execCreateTransportFailure(err, host.ID, req.RequestKey, target), nil
	}
	receipt, failOut, ok := c.decodeExecReceipt("sessions.create", resp, host.ID, req.RequestKey, target)
	if !ok {
		return failOut, nil
	}
	ref := mcpnative.JobRef{HostID: receipt.HostID, SessionID: receipt.SessionID, Instance: receipt.Instance}
	receipt, waitStatus, err := c.finishExecWait(ctx, host, req, ref, receipt, notify)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	stdoutData, stderrData := c.fetchExecInitialStreams(ctx, host, req, ref)
	return okOutcome("sessions.create", target, mcpnative.ExecResultData{
		Job:        ref.String(),
		RequestKey: receipt.RequestKey,
		Target:     *target,
		State:      mcpnative.JobStateFromReceipt(receipt),
		Wait:       waitStatus,
		Result:     receipt.Result,
		Receipt:    receipt,
		Stdout:     stdoutData,
		Stderr:     stderrData,
	}), nil
}

func (c *mcpControls) finishExecWait(ctx context.Context, host HostRecord, req mcpnative.ExecRequest, ref mcpnative.JobRef, receipt *agentapi.ExecutionReceipt, notify func(mcpnative.ProgressUpdate)) (*agentapi.ExecutionReceipt, string, error) {
	if receipt.Phase == "terminal" || req.WaitMS <= 0 {
		return receipt, "", nil
	}
	waited, status, waitErr := c.waitExecJob(ctx, host, req, ref, notify)
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("wait for exec job cancelled: %w", ctx.Err())
		}
		return receipt, "timed_out", nil
	}
	if waited != nil {
		receipt = waited
	}
	return receipt, status, nil
}

func (c *mcpControls) execCreateTransportFailure(err error, hostID, key string, target *mcpnative.TargetMeta) mcpnative.CallOutcome {
	if isPreDispatchError(err) || errors.Is(err, transport.ErrDestinationIdentityMismatch) || errors.Is(err, transport.ErrAuthentication) {
		return failOutcome("sessions.create", target, agentError(err, "sessions.create", hostID, agentapi.HostUnreachable))
	}
	msg := fmt.Sprintf("Execution request %q outcome is uncertain (%v); query mesh_job_receipt with context and request_key.", key, err)
	return failOutcome("sessions.create", target, agentapi.Fail(agentapi.RequestOutcomeUnknown, "sessions.create", hostID, msg).Err)
}

func (c *mcpControls) decodeExecReceipt(operation string, resp protocol.Control, hostID, key string, target *mcpnative.TargetMeta) (*agentapi.ExecutionReceipt, mcpnative.CallOutcome, bool) {
	var partial *mcpnative.ExecResultData
	var receipt *agentapi.ExecutionReceipt
	if resp.ExecutionReceipt != nil {
		parsed, err := agentExecutionReceipt(resp.ExecutionReceipt, hostID, key)
		if err != nil {
			return nil, failOutcome(operation, target, agentError(err, operation, hostID, agentapi.InternalError)), false
		}
		receipt = parsed
		ref := mcpnative.JobRef{HostID: parsed.HostID, SessionID: parsed.SessionID, Instance: parsed.Instance}
		partial = &mcpnative.ExecResultData{
			Job:        ref.String(),
			RequestKey: parsed.RequestKey,
			Target:     *target,
			State:      mcpnative.JobStateFromReceipt(parsed),
			Result:     parsed.Result,
			Receipt:    parsed,
		}
	}
	if resp.Type == protocol.TypeError {
		failure := agentapi.Fail(controlAgentCode(resp.ErrorCode), operation, hostID, resp.Message).Err
		envelope := agentapi.Envelope{Operation: operation, Status: agentapi.StatusError, Errors: []agentapi.Error{failure}}
		if partial != nil {
			envelope.Data = *partial
		}
		return nil, mcpnative.CallOutcome{Envelope: envelope, Target: target, IsError: true}, false
	}
	if resp.Type != protocol.TypeExecutionReceipt || receipt == nil {
		return nil, failOutcome(operation, target, agentapi.Fail(agentapi.InternalError, operation, hostID, "host did not return an execution receipt").Err), false
	}
	return receipt, mcpnative.CallOutcome{}, true
}

func (c *mcpControls) waitExecJob(ctx context.Context, host HostRecord, req mcpnative.ExecRequest, ref mcpnative.JobRef, notify func(mcpnative.ProgressUpdate)) (*agentapi.ExecutionReceipt, string, error) {
	deadline := time.Now().Add(time.Duration(req.WaitMS) * time.Millisecond)
	stopProgress := c.startExecProgress(ctx, host, req.Context, ref, notify)
	defer stopProgress()
	resp, _, err := c.pool.ExchangeStreamBudgeted(ctx, host, deadline, func(timeoutMS int) protocol.Control {
		return protocol.Control{
			Type:             protocol.TypeExecutionWait,
			Caller:           c.caller,
			SessionID:        ref.SessionID,
			Instance:         ref.Instance,
			TimeoutMillis:    timeoutMS,
			ExecutionContext: &protocol.ExecutionContextRequest{ContextID: req.Context.ContextID},
		}
	})
	if err != nil {
		return nil, "", err
	}
	if resp.ExecutionReceipt == nil {
		return nil, resp.ExecutionWait, nil
	}
	parsed, parseErr := agentExecutionReceipt(resp.ExecutionReceipt, host.ID, req.RequestKey)
	if parseErr != nil {
		return nil, resp.ExecutionWait, parseErr
	}
	return parsed, resp.ExecutionWait, nil
}

func (c *mcpControls) startExecProgress(ctx context.Context, host HostRecord, ctxRef mcpnative.ContextRef, ref mcpnative.JobRef, notify func(mcpnative.ProgressUpdate)) func() {
	if notify == nil {
		return func() {}
	}
	done := make(chan struct{})
	notify(mcpnative.ProgressUpdate{Progress: 0, Message: "running " + ref.String() + " (stdout=0B stderr=0B)"})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				stdoutBytes := c.probeObservedBytes(ctx, host, ctxRef, ref, execution.StreamStdout)
				stderrBytes := c.probeObservedBytes(ctx, host, ctxRef, ref, execution.StreamStderr)
				totalBytes := float64(stdoutBytes + stderrBytes)
				notify(mcpnative.ProgressUpdate{
					Progress: totalBytes,
					Message:  fmt.Sprintf("running %s (stdout=%dB stderr=%dB)", ref.String(), stdoutBytes, stderrBytes),
				})
			}
		}
	}()
	return func() { close(done) }
}

func (c *mcpControls) probeObservedBytes(ctx context.Context, host HostRecord, ctxRef mcpnative.ContextRef, ref mcpnative.JobRef, stream string) uint64 {
	probeCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	ctrl := protocol.Control{
		Type:             protocol.TypeExecutionLogs,
		Caller:           c.caller,
		SessionID:        ref.SessionID,
		Instance:         ref.Instance,
		Stream:           stream,
		MaxBytes:         1,
		ExecutionContext: &protocol.ExecutionContextRequest{ContextID: ctxRef.ContextID},
	}
	resp, _, err := c.pool.ExchangeShort(probeCtx, host, ctrl, false)
	if err != nil || resp.ExecutionPage == nil {
		return 0
	}
	n, err := strconv.ParseUint(resp.ExecutionPage.ObservedEnd, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func (c *mcpControls) fetchExecInitialStreams(ctx context.Context, host HostRecord, req mcpnative.ExecRequest, ref mcpnative.JobRef) (*mcpnative.ExecStreamData, *mcpnative.ExecStreamData) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stdoutData := c.fetchOneExecStream(readCtx, host, req.Context, ref, execution.StreamStdout, req.OutputBytes)
	stderrData := c.fetchOneExecStream(readCtx, host, req.Context, ref, execution.StreamStderr, req.OutputBytes)
	return stdoutData, stderrData
}

func (c *mcpControls) fetchOneExecStream(ctx context.Context, host HostRecord, ctxRef mcpnative.ContextRef, ref mcpnative.JobRef, stream string, maxBytes int) *mcpnative.ExecStreamData {
	ctrl := protocol.Control{
		Type:             protocol.TypeExecutionLogs,
		Caller:           c.caller,
		SessionID:        ref.SessionID,
		Instance:         ref.Instance,
		Stream:           stream,
		MaxBytes:         maxBytes,
		ExecutionContext: &protocol.ExecutionContextRequest{ContextID: ctxRef.ContextID},
	}
	resp, _, err := c.pool.ExchangeShort(ctx, host, ctrl, true)
	if err != nil || resp.ExecutionPage == nil {
		return nil
	}
	page, err := agentExecutionPage(resp.ExecutionPage, host.ID, ctrl)
	if err != nil {
		return nil
	}
	return mcpnative.BuildExecStreamData(page)
}
