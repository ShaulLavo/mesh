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
// exactly as execution.wait would and, once the job has finished, attaches the
// first page of each output stream as execution.logs without a cursor would,
// so a short command costs
// the caller one round trip instead of three or four. Every check of those
// controls applies (the exact binding, the caller, the device grant), and the
// grant is checked again once the wait ends. The page reads reuse the wait's
// admission of this caller to this exact job instead of repeating it for each
// stream, which cost about 10 ms a page on a Raspberry Pi. The create is
// already decided here, so a failed observation leaves the create reply
// unchanged and the caller continues with separate wait and logs requests. A
// replay of the same key reaches this point through the receipt lookup and
// observes the same job.
func (l *lifecycle) withInlineResult(ctx context.Context, request, response protocol.Control, err error) (protocol.Control, error) {
	if err != nil || request.Type != protocol.TypeExecutionCreate || request.WaitMillis == 0 || response.ExecutionReceipt == nil {
		return response, err
	}
	// The budget starts at the create's own reply point, not after the reads
	// that resolve the binding. The wait, those reads included, ends a bounded
	// grace after it, so it cannot use up the caller's deadline for the create
	// reply, while even a 1 ms wait still gets its first look. The grant check
	// and page reads after it have their own bounds.
	deadline := time.Now().Add(time.Duration(request.WaitMillis) * time.Millisecond)
	ctx, stop := observeUntilDisconnect(ctx)
	defer stop()
	waitCtx, cancel := context.WithDeadline(ctx, deadline.Add(inlineObservationGrace))
	defer cancel()
	bound := inlineBound(request, response.ExecutionReceipt)
	waited, job, ok := l.inlineWait(waitCtx, bound, deadline)
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
	// The grant check and the page reads use the request's own context: an
	// expired wait budget must not turn a valid result into an error, and the
	// pages have their own budget.
	if err := workerExecutionAdmission(ctx); err != nil {
		return response, err
	}
	if request.OutputBytes == 0 {
		return waited, nil
	}
	pageCtx, cancelPages := context.WithTimeout(ctx, inlinePageBudget)
	defer cancelPages()
	waited.ExecutionOutput = l.inlinePages(pageCtx, bound, job, inlineStreams(response.ExecutionReceipt.Kind), request.OutputBytes)
	return waited, nil
}

// inlineAfterWait lets a test act between the inline wait and the grant check.
var inlineAfterWait = func() {}

// inlineBetweenPages lets a test act after one inline page is read and
// before the next.
var inlineBetweenPages = func() {}

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

// inlineWait returns the wait's reply and the job it admitted the caller to;
// ok reports a reply for exactly the created job.
func (l *lifecycle) inlineWait(ctx context.Context, bound protocol.Control, deadline time.Time) (protocol.Control, boundJob, bool) {
	// Always look at least once, rounding up: even a 1 ms budget, or one the
	// create's own work used up, still observes a job that has finished.
	millis := max(1, int((time.Until(deadline)+time.Millisecond-1)/time.Millisecond))
	// The wait bounds itself by TimeoutMillis; the reads around it keep their
	// own lock budgets rather than failing on a deadline meant for the wait.
	bound.Type, bound.TimeoutMillis = protocol.TypeExecutionWait, millis
	job, answer, _ := l.admitBoundExecution(ctx, bound)
	if job.journal == nil {
		return answer, job, false
	}
	waited, err := l.actOnBoundExecution(ctx, bound, job)
	receipt := waited.ExecutionReceipt
	return waited, job, err == nil && receipt != nil && receipt.SessionID == bound.SessionID && receipt.Instance == bound.Instance
}

// inlinePages reads the first page of each stream as execution.logs without
// a cursor would. A stream whose page cannot be read is left out; each page
// names its stream, so the caller reads only the missing one itself.
func (l *lifecycle) inlinePages(ctx context.Context, bound protocol.Control, job boundJob, streams []string, size int) []protocol.ExecutionPage {
	var pages []protocol.ExecutionPage
	for i, stream := range streams {
		if i > 0 {
			inlineBetweenPages()
		}
		// Each page rereads the exact receipt, as a separate execution.logs
		// would: retirement and eviction mark it before they delete stream
		// bytes, so a page read after that reports the output as expired.
		r, err := job.journal.Peek(ctx, job.receipt.Binding)
		if err != nil {
			return pages
		}
		// A live receipt may lag a result published after the wait ended,
		// which changes how a page reports its stream; only a separate read
		// refreshes it, so the caller reads a live job's pages itself.
		if r.Phase != executionTerminalPhase {
			return nil
		}
		req := bound
		req.Type, req.Stream, req.MaxBytes = protocol.TypeExecutionLogs, stream, size
		response, _ := executionResponse(req, r, nil)
		reply, err := l.executionLogs(ctx, req, r, response)
		if err == nil && reply.ExecutionPage != nil {
			pages = append(pages, *reply.ExecutionPage)
		}
	}
	return pages
}

func inlineStreams(kind string) []string {
	if kind == execution.KindPTY {
		return []string{execution.KindPTY}
	}
	return []string{execution.StreamStdout, execution.StreamStderr}
}
