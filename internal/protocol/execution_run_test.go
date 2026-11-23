package protocol

import (
	"testing"
	"time"
)

func runResultWith(change func(*ExecutionRunResult)) ExecutionRunResult {
	code := 0
	eof := ExecutionRunStream{CloseReason: "eof"}
	r := ExecutionRunResult{Termination: ExecutionTermination{Kind: "exit", Code: &code}, Cwd: "/work", ContextRevision: 1, StartedAt: time.Now(), DurationMillis: 3, Stdout: eof, Stderr: eof}
	change(&r)
	return r
}

func TestExecutionRunResultValidate(t *testing.T) {
	code, signal, zero, big := 0, 15, 0, 256
	valid := map[string]ExecutionRunResult{
		"exit": runResultWith(func(r *ExecutionRunResult) {
			r.Stdout = ExecutionRunStream{Bytes: []byte("abcd"), Total: 10, Truncated: true, CloseReason: "eof"}
		}),
		"signal after a deadline": runResultWith(func(r *ExecutionRunResult) {
			r.Termination, r.TimedOut = ExecutionTermination{Kind: "signal", Signal: &signal}, true
		}),
		"interrupted": runResultWith(func(r *ExecutionRunResult) {
			r.Termination = ExecutionTermination{Kind: "interrupted", Reason: "wait_failed"}
		}),
		"not started": runResultWith(func(r *ExecutionRunResult) {
			r.Termination, r.DurationMillis = ExecutionTermination{Kind: "not_started", Reason: "not found"}, 0
			r.Stdout, r.Stderr = ExecutionRunStream{}, ExecutionRunStream{}
		}),
	}
	for name, result := range valid {
		if err := result.Validate(4); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	invalid := map[string]ExecutionRunResult{
		"empty":               {},
		"signal 0, no fields": {Termination: ExecutionTermination{Kind: "signal", Signal: &zero}},
		"signal 0":            runResultWith(func(r *ExecutionRunResult) { r.Termination = ExecutionTermination{Kind: "signal", Signal: &zero} }),
		"exit 256":            runResultWith(func(r *ExecutionRunResult) { r.Termination.Code = &big }),
		"exit with a reason":  runResultWith(func(r *ExecutionRunResult) { r.Termination.Reason = "x" }),
		"signal with a code": runResultWith(func(r *ExecutionRunResult) {
			r.Termination = ExecutionTermination{Kind: "signal", Signal: &signal, Code: &code}
		}),
		"interrupted without a reason": runResultWith(func(r *ExecutionRunResult) { r.Termination = ExecutionTermination{Kind: "interrupted"} }),
		"no cwd":                       runResultWith(func(r *ExecutionRunResult) { r.Cwd = "" }),
		"relative cwd":                 runResultWith(func(r *ExecutionRunResult) { r.Cwd = "work" }),
		"no revision":                  runResultWith(func(r *ExecutionRunResult) { r.ContextRevision = 0 }),
		"no start":                     runResultWith(func(r *ExecutionRunResult) { r.StartedAt = time.Time{} }),
		"no close reason":              runResultWith(func(r *ExecutionRunResult) { r.Stderr.CloseReason = "" }),
		"unknown close reason":         runResultWith(func(r *ExecutionRunResult) { r.Stderr.CloseReason = "gone" }),
		"more bytes than asked":        runResultWith(func(r *ExecutionRunResult) { r.Stdout.Bytes, r.Stdout.Total = []byte("abcde"), 5 }),
		"total below the bytes":        runResultWith(func(r *ExecutionRunResult) { r.Stdout.Bytes, r.Stdout.Total = []byte("ab"), 1 }),
		"short prefix": runResultWith(func(r *ExecutionRunResult) {
			r.Stdout = ExecutionRunStream{Bytes: []byte("abc"), Total: 10, Truncated: true, CloseReason: "eof"}
		}),
		"short complete stream": runResultWith(func(r *ExecutionRunResult) { r.Stdout.Bytes, r.Stdout.Total = []byte("a"), 2 }),
		"truncation disagrees": runResultWith(func(r *ExecutionRunResult) {
			r.Stdout.Bytes, r.Stdout.Total, r.Stdout.Truncated = []byte("ab"), 2, true
		}),
		"not started with output": runResultWith(func(r *ExecutionRunResult) {
			r.Termination, r.DurationMillis = ExecutionTermination{Kind: "not_started", Reason: "x"}, 0
		}),
		"not started without a reason": runResultWith(func(r *ExecutionRunResult) {
			r.Termination, r.DurationMillis = ExecutionTermination{Kind: "not_started"}, 0
			r.Stdout, r.Stderr = ExecutionRunStream{}, ExecutionRunStream{}
		}),
		"not started but timed out": runResultWith(func(r *ExecutionRunResult) {
			r.Termination, r.DurationMillis, r.TimedOut = ExecutionTermination{Kind: "not_started", Reason: "x"}, 0, true
			r.Stdout, r.Stderr = ExecutionRunStream{}, ExecutionRunStream{}
		}),
	}
	for name, result := range invalid {
		if err := result.Validate(4); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var missing *ExecutionRunResult
	if missing.Validate(4) == nil {
		t.Error("a missing result was accepted")
	}
}
