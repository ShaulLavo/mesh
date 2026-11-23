package daemon

import (
	"context"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

// inlinePageBudget bounds both page reads of one inline observation, so a
// busy output stream cannot hold a create reply long after its wait ended.
const inlinePageBudget = 2 * time.Second

// inlineObservationGrace is how long past its wait budget an observation may
// still spend resolving the binding and confirming the result.
const inlineObservationGrace = time.Second

// withInlineResult finishes a create that asked to wait. It waits for the job
// exactly as execution.wait would and attaches the first page of each output
// stream as execution.logs without a cursor would, so a short command costs
// the caller one round trip instead of three or four. Every check of those
// controls applies (the exact binding, the caller, the device grant), and the
// grant is checked again once the wait ends. The create is already decided
// here, so a failed observation leaves the create reply unchanged and the
// caller continues with separate wait and logs requests. A replay of the same
// key reaches this point through the receipt lookup and observes the same job.
func (l *lifecycle) withInlineResult(ctx context.Context, request, response protocol.Control, err error) (protocol.Control, error) {
	if err != nil || request.Type != protocol.TypeExecutionCreate || request.WaitMillis == 0 || response.ExecutionReceipt == nil {
		return response, err
	}
	// The budget starts at the create's own reply point, not after the reads
	// that resolve the binding. The whole observation, those reads included,
	// ends a bounded grace after it, so it cannot use up the caller's deadline
	// for the create reply, while even a 1 ms wait still gets its first look.
	deadline := time.Now().Add(time.Duration(request.WaitMillis) * time.Millisecond)
	ctx, stop := observeUntilDisconnect(ctx)
	defer stop()
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(inlineObservationGrace))
	defer cancel()
	bound := inlineBound(request, response.ExecutionReceipt)
	waited, ok := l.inlineWait(ctx, bound, deadline)
	if !ok && time.Now().After(deadline) {
		// The wait's budget is spent even though no answer came back, so the
		// caller charges it as a timeout instead of waiting the whole time again.
		response.ExecutionWait = executionWaitTimedOut
		return response, nil
	}
	if !ok {
		return response, nil
	}
	inlineAfterWait()
	if err := workerExecutionAdmission(ctx); err != nil {
		return response, err
	}
	if request.OutputBytes == 0 {
		return waited, nil
	}
	pageCtx, cancel := context.WithTimeout(ctx, inlinePageBudget)
	defer cancel()
	// A stream whose page cannot be read is left out; each page names its
	// stream, so the caller reads only the missing one itself.
	for _, stream := range inlineStreams(response.ExecutionReceipt.Kind) {
		if page, ok := l.inlinePage(pageCtx, bound, stream, request.OutputBytes); ok {
			waited.ExecutionOutput = append(waited.ExecutionOutput, page)
		}
	}
	return waited, nil
}

// inlineAfterWait lets a test act between the inline wait and the grant check.
var inlineAfterWait = func() {}

// inlineDisconnectKey carries a channel that closes when the client of a
// waiting create disconnects. Only the observation listens to it: the create
// itself, before and after its acceptance barrier, keeps its own rules.
type inlineDisconnectKey struct{}

func observeUntilDisconnect(ctx context.Context) (context.Context, context.CancelFunc) {
	observed, cancel := context.WithCancel(ctx)
	gone, ok := ctx.Value(inlineDisconnectKey{}).(<-chan struct{})
	if !ok {
		return observed, cancel
	}
	go func() {
		select {
		case <-gone:
			cancel()
		case <-observed.Done():
		}
	}()
	return observed, cancel
}

// inlineBound addresses the created job exactly, with the create's caller.
func inlineBound(request protocol.Control, receipt *protocol.ExecutionReceipt) protocol.Control {
	bound := protocol.Control{RequestID: request.RequestID, Caller: request.Caller, Administration: request.Administration, SessionID: receipt.SessionID, Instance: receipt.Instance}
	if request.ExecutionContext != nil {
		bound.ExecutionContext = &protocol.ExecutionContextRequest{ContextID: request.ExecutionContext.ContextID}
	}
	return bound
}

func (l *lifecycle) inlineWait(ctx context.Context, bound protocol.Control, deadline time.Time) (protocol.Control, bool) {
	// Always look at least once, rounding up: even a 1 ms budget, or one the
	// create's own work used up, still observes a job that has finished.
	millis := max(1, int((time.Until(deadline)+time.Millisecond-1)/time.Millisecond))
	// The wait bounds itself by TimeoutMillis; the reads around it keep their
	// own lock budgets rather than failing on a deadline meant for the wait.
	bound.Type, bound.TimeoutMillis = protocol.TypeExecutionWait, millis
	waited, err := l.boundExecutionControl(ctx, bound)
	receipt := waited.ExecutionReceipt
	return waited, err == nil && receipt != nil && receipt.SessionID == bound.SessionID && receipt.Instance == bound.Instance
}

func (l *lifecycle) inlinePage(ctx context.Context, bound protocol.Control, stream string, size int) (protocol.ExecutionPage, bool) {
	bound.Type, bound.Stream, bound.MaxBytes = protocol.TypeExecutionLogs, stream, size
	reply, err := l.boundExecutionControl(ctx, bound)
	if err != nil || reply.ExecutionPage == nil {
		return protocol.ExecutionPage{}, false
	}
	return *reply.ExecutionPage, true
}

func inlineStreams(kind string) []string {
	if kind == execution.KindPTY {
		return []string{execution.KindPTY}
	}
	return []string{execution.StreamStdout, execution.StreamStderr}
}
