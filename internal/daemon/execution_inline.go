package daemon

import (
	"context"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

// withInlineResult finishes a create that asked to wait. It waits for the job
// exactly as execution.wait would and attaches the first page of each output
// stream as execution.logs without a cursor would, so a short command costs
// the caller one round trip instead of three or four. Every check of those
// controls applies: the exact binding, the caller, and the device grant after
// the wait. The create is already decided here, so a failed observation
// leaves the create reply unchanged and the caller continues with separate
// wait and logs requests. A replay of the same key reaches this point through
// the receipt lookup and observes the same job again.
func (l *lifecycle) withInlineResult(ctx context.Context, request, response protocol.Control, err error) (protocol.Control, error) {
	if err != nil || request.Type != protocol.TypeExecutionCreate || request.WaitMillis == 0 || response.ExecutionReceipt == nil {
		return response, err
	}
	bound := inlineBound(request, response.ExecutionReceipt)
	waited, ok := l.inlineWait(ctx, bound, request.WaitMillis)
	if !ok {
		return response, nil
	}
	if request.OutputBytes == 0 {
		return waited, nil
	}
	// A stream whose page cannot be read is left out; each page names its
	// stream, so the caller reads only the missing one itself.
	for _, stream := range inlineStreams(response.ExecutionReceipt.Kind) {
		if page, ok := l.inlinePage(ctx, bound, stream, request.OutputBytes); ok {
			waited.ExecutionOutput = append(waited.ExecutionOutput, page)
		}
	}
	return waited, nil
}

// inlineBound addresses the created job exactly, with the create's caller.
func inlineBound(request protocol.Control, receipt *protocol.ExecutionReceipt) protocol.Control {
	bound := protocol.Control{RequestID: request.RequestID, Caller: request.Caller, Administration: request.Administration, SessionID: receipt.SessionID, Instance: receipt.Instance}
	if request.ExecutionContext != nil {
		bound.ExecutionContext = &protocol.ExecutionContextRequest{ContextID: request.ExecutionContext.ContextID}
	}
	return bound
}

func (l *lifecycle) inlineWait(ctx context.Context, bound protocol.Control, millis int) (protocol.Control, bool) {
	bound.Type, bound.TimeoutMillis = protocol.TypeExecutionWait, millis
	waited, err := l.boundExecutionControl(ctx, bound)
	return waited, err == nil && waited.ExecutionReceipt != nil
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
