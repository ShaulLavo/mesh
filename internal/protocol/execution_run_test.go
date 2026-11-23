package protocol

import "testing"

func TestExecutionRunResultValidate(t *testing.T) {
	code, signal := 0, 15
	stream := ExecutionRunStream{Bytes: []byte("abc"), Total: 10, Truncated: true, CloseReason: "eof"}
	valid := []ExecutionRunResult{
		{Termination: ExecutionTermination{Kind: "exit", Code: &code}, Stdout: stream},
		{Termination: ExecutionTermination{Kind: "signal", Signal: &signal}},
		{Termination: ExecutionTermination{Kind: "not_started", Reason: "not found"}},
	}
	for _, result := range valid {
		if err := result.Validate(4); err != nil {
			t.Fatalf("%+v: %v", result, err)
		}
	}
	invalid := map[string]ExecutionRunResult{
		"empty termination":     {},
		"exit without a code":   {Termination: ExecutionTermination{Kind: "exit"}},
		"signal with a code":    {Termination: ExecutionTermination{Kind: "signal", Signal: &signal, Code: &code}},
		"more bytes than asked": {Termination: ExecutionTermination{Kind: "exit", Code: &code}, Stdout: ExecutionRunStream{Bytes: []byte("abcde"), Total: 5}},
		"total below the bytes": {Termination: ExecutionTermination{Kind: "exit", Code: &code}, Stdout: ExecutionRunStream{Bytes: []byte("ab"), Total: 1}},
		"truncation disagrees":  {Termination: ExecutionTermination{Kind: "exit", Code: &code}, Stdout: ExecutionRunStream{Bytes: []byte("ab"), Total: 2, Truncated: true}},
		"unknown close reason":  {Termination: ExecutionTermination{Kind: "exit", Code: &code}, Stderr: ExecutionRunStream{CloseReason: "gone"}},
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
