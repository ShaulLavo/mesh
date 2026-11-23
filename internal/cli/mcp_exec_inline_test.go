package cli

import (
	"testing"

	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

func TestInlineExecStreamsUsesOnlyCompletedValidPages(t *testing.T) {
	ref := mcpnative.JobRef{HostID: "host", SessionID: "7K3D", Instance: "inst"}
	page := func(stream string, data string) protocol.ExecutionPage {
		return protocol.ExecutionPage{HostID: "host", SessionID: "7K3D", Instance: "inst", Stream: stream, Bytes: []byte(data), EOF: true}
	}
	completed := protocol.Control{ExecutionWait: "completed", ExecutionOutput: []protocol.ExecutionPage{page(execution.StreamStdout, "out"), page(execution.StreamStderr, "err")}}
	if got := inlineExecStreams(completed, "host", ref, 16); len(got) != 2 || got[execution.StreamStdout] == nil || got[execution.StreamStderr] == nil {
		t.Fatalf("completed pages = %+v", got)
	}
	timedOut := completed
	timedOut.ExecutionWait = "timed_out"
	if got := inlineExecStreams(timedOut, "host", ref, 16); len(got) != 0 {
		t.Fatalf("a snapshot from before the wait was used: %+v", got)
	}
	// A page for another instance, or larger than asked, is read again instead.
	foreign := page(execution.StreamStderr, "err")
	foreign.Instance = "other"
	mixed := protocol.Control{ExecutionWait: "completed", ExecutionOutput: []protocol.ExecutionPage{page(execution.StreamStdout, "too long"), foreign}}
	if got := inlineExecStreams(mixed, "host", ref, 4); len(got) != 0 {
		t.Fatalf("invalid pages were used: %+v", got)
	}
}

func TestRemainingExecWaitChargesTheInlineWait(t *testing.T) {
	req := mcpnative.ExecRequest{WaitMS: 5000}
	for _, wait := range []string{"", "completed"} {
		if got := remainingExecWait(req, 1000, protocol.Control{ExecutionWait: wait}); got.WaitMS != 5000 {
			t.Fatalf("create with wait %q changed the budget: %d", wait, got.WaitMS)
		}
	}
	if got := remainingExecWait(req, 1000, protocol.Control{ExecutionWait: "timed_out"}); got.WaitMS != 4000 {
		t.Fatalf("remaining wait = %d, want 4000", got.WaitMS)
	}
	if got := remainingExecWait(mcpnative.ExecRequest{WaitMS: 600}, 1000, protocol.Control{ExecutionWait: "timed_out"}); got.WaitMS != 0 {
		t.Fatalf("an exhausted budget = %d, want 0", got.WaitMS)
	}
}
