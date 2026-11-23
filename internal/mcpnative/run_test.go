package mcpnative

import (
	"context"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// runStub records the last mesh_run request. It is embedded in stubControls.
type runStub struct {
	lastRun *RunRequest
}

func (s *runStub) Run(_ context.Context, req RunRequest) (CallOutcome, error) {
	s.lastRun = &req
	code := 0
	result := &protocol.ExecutionRunResult{Termination: protocol.ExecutionTermination{Kind: "exit", Code: &code}, Stdout: protocol.ExecutionRunStream{Bytes: []byte("hi\n"), Total: 3, CloseReason: "eof"}}
	return CallOutcome{Envelope: okEnvelope(RunOperation, BuildRunResult(result, TargetMeta{HostID: testHostID}))}, nil
}

func TestRunToolParsesItsArguments(t *testing.T) {
	stub := &stubControls{}
	out, err := callFileTool(t, stub, "mesh_run", `{"context":"`+testContextHandle+`","command":"git status","cwd":"sub","env":{"A":"b"}}`)
	if err != nil {
		t.Fatal(err)
	}
	got := stub.lastRun
	if got == nil || got.Command != "git status" || got.Cwd != "sub" || got.Env["A"] != "b" || got.TimeoutMS != defaultWaitMS || got.OutputBytes != defaultExecOutput {
		t.Fatalf("request = %+v", got)
	}
	if text := renderOutcomeText(out.Envelope, false); !strings.Contains(text, "run: exited termination=exit code=0") || !strings.Contains(text, "hi") {
		t.Fatalf("rendered = %q", text)
	}
}

func TestRunToolRejectsBadArguments(t *testing.T) {
	requireInvalid(t, "mesh_run", `{"context":"`+testContextHandle+`"}`, "exactly one of command or argv")
	requireInvalid(t, "mesh_run", `{"context":"`+testContextHandle+`","command":"x","timeout_ms":99}`, "timeout_ms")
	requireInvalid(t, "mesh_run", `{"context":"`+testContextHandle+`","command":"x","output_bytes":262145}`, "output_bytes")
	requireInvalid(t, "mesh_run", `{"context":"`+testContextHandle+`","command":"x","request_key":"k"}`, "request_key")
}

func TestRunResultStatesAndSafeText(t *testing.T) {
	signal := 15
	timedOut := BuildRunResult(&protocol.ExecutionRunResult{TimedOut: true, Termination: protocol.ExecutionTermination{Kind: "signal", Signal: &signal}, Stdout: protocol.ExecutionRunStream{Bytes: []byte("\x1b[2Jcleared"), Total: 900, Truncated: true, CloseReason: "eof"}}, TargetMeta{})
	if timedOut.State != RunStateTimedOut || timedOut.Stdout.TotalBytes != 900 || !timedOut.Stdout.Truncated {
		t.Fatalf("timed out = %+v", timedOut)
	}
	text := renderOutcomeText(agentapi.Envelope{Operation: RunOperation, Status: agentapi.StatusOK, Data: timedOut}, false)
	if strings.Contains(text, "\x1b") || !strings.Contains(text, "truncated") {
		t.Fatalf("rendered text = %q", text)
	}
	binary := BuildRunResult(&protocol.ExecutionRunResult{Termination: protocol.ExecutionTermination{Kind: "exit"}, Stdout: protocol.ExecutionRunStream{Bytes: []byte{0, 0xff}, Total: 2}}, TargetMeta{})
	if binary.Stdout.Encoding != "base64" || binary.Stdout.Data != "AP8=" {
		t.Fatalf("binary stream = %+v", binary.Stdout)
	}
	notStarted := BuildRunResult(&protocol.ExecutionRunResult{Termination: protocol.ExecutionTermination{Kind: RunStateNotStarted, Reason: "executable not found"}}, TargetMeta{})
	if notStarted.State != RunStateNotStarted {
		t.Fatalf("not started = %+v", notStarted)
	}
}

func TestRunHintsNameNoReceipt(t *testing.T) {
	hint := nextActionHint(RunOperation, agentapi.RequestOutcomeUnknown)
	if !strings.Contains(hint, "no receipt") || strings.Contains(hint, "mesh_job_receipt") {
		t.Fatalf("lost-reply hint = %q", hint)
	}
	if hint := nextActionHint(RunOperation, agentapi.ResourceLimit); strings.Contains(hint, "mesh_context_close") {
		t.Fatalf("limit hint = %q", hint)
	}
}
