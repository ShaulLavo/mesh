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
		return executionObject(raw)
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

func executionRange(name string, raw json.RawMessage, low, high int) error {
	var n int
	if json.Unmarshal(raw, &n) != nil || n < low || n > high {
		return fmt.Errorf("execution: %s must be %d to %d", name, low, high)
	}
	return nil
}
