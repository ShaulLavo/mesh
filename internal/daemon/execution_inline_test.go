package daemon

import (
	"bytes"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

func TestInlineCreateReturnsResultAndOutputOnce(t *testing.T) {
	f := newNativePipeDaemon(t)
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "inline", RequestKey: "inline", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf out; printf err >&2; exit 3"}, WaitMillis: 10000, OutputBytes: 4096}
	first := f.request(t, req)
	assertInlineCompletion(t, first, 3, "out", "err")
	// A replay of the same key observes the same job; it never launches again.
	replay := f.request(t, req)
	assertInlineCompletion(t, replay, 3, "out", "err")
	if replay.ExecutionReceipt.Instance != first.ExecutionReceipt.Instance {
		t.Fatalf("replay changed instance: %s then %s", first.ExecutionReceipt.Instance, replay.ExecutionReceipt.Instance)
	}
	conflict := req
	conflict.Command = []string{"/bin/true"}
	reply, err := f.exchange(conflict)
	if err != nil || reply.Type != protocol.TypeError || reply.ErrorCode != "request.key_conflict" || len(reply.ExecutionOutput) != 0 {
		t.Fatalf("conflicting replay = %+v, %v", reply, err)
	}
}

func TestInlineCreateTimesOutWithTheLiveReceipt(t *testing.T) {
	f := newNativePipeDaemon(t)
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "slow", RequestKey: "slow", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf started; exec sleep 30"}, WaitMillis: 200, OutputBytes: 64}
	reply := f.request(t, req)
	if reply.ExecutionWait != "timed_out" || reply.ExecutionReceipt == nil || reply.ExecutionReceipt.Phase == "terminal" || reply.ExecutionReceipt.Result != nil {
		t.Fatalf("slow inline create = %+v", reply)
	}
	for _, page := range reply.ExecutionOutput {
		if page.EOF {
			t.Fatalf("live stream %s reported EOF", page.Stream)
		}
	}
	// Without waitMillis a create answers as before, with no observation.
	plain := req
	plain.RequestKey, plain.WaitMillis, plain.OutputBytes = "plain", 0, 0
	if reply := f.request(t, plain); reply.ExecutionWait != "" || reply.ExecutionOutput != nil {
		t.Fatalf("plain create carried an observation: %+v", reply)
	}
}

func assertInlineCompletion(t *testing.T, reply protocol.Control, code int, stdout, stderr string) {
	t.Helper()
	r := reply.ExecutionReceipt
	if reply.ExecutionWait != "completed" || r == nil || r.Phase != "terminal" || r.Result == nil || r.Result.Termination.Code == nil || *r.Result.Termination.Code != code {
		t.Fatalf("inline completion = %+v", reply)
	}
	want := map[string]string{execution.StreamStdout: stdout, execution.StreamStderr: stderr}
	if len(reply.ExecutionOutput) != len(want) {
		t.Fatalf("inline pages = %+v", reply.ExecutionOutput)
	}
	for _, page := range reply.ExecutionOutput {
		if page.SessionID != r.SessionID || page.Instance != r.Instance || !page.EOF || !bytes.Equal(page.Bytes, []byte(want[page.Stream])) {
			t.Fatalf("inline page %s = %+v", page.Stream, page)
		}
	}
}
