package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Limits of one execution.run. The deadline has the MCP tool's range, and
// two full streams, base64 encoded, keep the reply under 1 MiB.
const (
	MinExecutionRunTimeoutMillis = 100
	MaxExecutionRunTimeoutMillis = 60000
	MaxExecutionRunOutput        = 256 << 10
)

const (
	timeoutMillisField = "timeoutMillis"
	outputBytesField   = "outputBytes"
)

// ExecutionRunResult answers one execution.run. Termination is the leader's
// own exit or signal, or kind not_started with the reason it could not
// start. TimedOut says the deadline fired, whatever the leader reported.
type ExecutionRunResult struct {
	Termination     ExecutionTermination `json:"termination"`
	TimedOut        bool                 `json:"timedOut"`
	Cwd             string               `json:"cwd"`
	ContextRevision uint64               `json:"contextRevision,string"`
	StartedAt       time.Time            `json:"startedAt"`
	DurationMillis  int64                `json:"durationMillis"`
	Stdout          ExecutionRunStream   `json:"stdout"`
	Stderr          ExecutionRunStream   `json:"stderr"`
}

// ExecutionRunStream holds the first bytes of one stream and the number of
// bytes the daemon read from it in all.
type ExecutionRunStream struct {
	Bytes       []byte `json:"bytesBase64"`
	Total       int64  `json:"total,string"`
	Truncated   bool   `json:"truncated"`
	CloseReason string `json:"closeReason"`
}

func validateExecutionRunField(name string, raw json.RawMessage) error {
	switch name {
	case executionTypeField, requestIDField, callerField, "cwd":
		return executionString(raw)
	case executionContextField:
		return executionRunContext(raw)
	case "command":
		return executionArguments(raw)
	case timeoutMillisField:
		return executionRange(name, raw, MinExecutionRunTimeoutMillis, MaxExecutionRunTimeoutMillis)
	case outputBytesField:
		return executionRange(name, raw, 1, MaxExecutionRunOutput)
	default:
		return fmt.Errorf("execution: unexpected field %s", name)
	}
}

// executionRunContext admits only what a run uses from executionContext, so
// a field such as expectedRevision is refused rather than ignored.
func executionRunContext(raw json.RawMessage) error {
	if err := executionObject(raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for name, value := range fields {
		if err := executionRunContextField(name, value); err != nil {
			return err
		}
	}
	return nil
}

func executionRunContextField(name string, raw json.RawMessage) error {
	switch name {
	case "contextId", "shellCommand":
		return executionString(raw)
	case "env":
		var env map[string]string
		if json.Unmarshal(raw, &env) != nil {
			return fmt.Errorf("execution: invalid run env")
		}
		return nil
	default:
		return fmt.Errorf("execution: unexpected executionContext field %s", name)
	}
}

// Validate checks a run reply against the request it answers: a known
// termination with only the fields its kind has, and streams within the
// requested size whose totals and flags agree.
func (r *ExecutionRunResult) Validate(outputBytes int) error {
	if r == nil {
		return fmt.Errorf("execution: run reply has no result")
	}
	if err := r.Termination.validateRun(); err != nil {
		return err
	}
	for _, s := range []ExecutionRunStream{r.Stdout, r.Stderr} {
		if err := s.validate(outputBytes); err != nil {
			return err
		}
	}
	return nil
}

func (t ExecutionTermination) validateRun() error {
	valid := false
	switch t.Kind {
	case "exit":
		valid = t.Code != nil && t.Signal == nil
	case "signal":
		valid = t.Signal != nil && t.Code == nil
	case "not_started", "interrupted":
		valid = t.Code == nil && t.Signal == nil
	}
	if !valid {
		return fmt.Errorf("execution: invalid run termination %q", t.Kind)
	}
	return nil
}

func (s ExecutionRunStream) validate(outputBytes int) error {
	kept := int64(len(s.Bytes))
	if kept > int64(outputBytes) || s.Total < kept || s.Truncated != (s.Total > kept) {
		return fmt.Errorf("execution: invalid run stream bounds")
	}
	switch s.CloseReason {
	case "", "eof", "drain_timeout", "read_error":
		return nil
	default:
		return fmt.Errorf("execution: invalid run stream close reason %q", s.CloseReason)
	}
}

func executionRange(name string, raw json.RawMessage, low, high int) error {
	var n int
	if json.Unmarshal(raw, &n) != nil || n < low || n > high {
		return fmt.Errorf("execution: %s must be %d to %d", name, low, high)
	}
	return nil
}
