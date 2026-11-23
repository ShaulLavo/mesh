package mcpnative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// RunOperation is mesh_run's operation: a request-scoped command with no
// receipt.
const RunOperation = "commands.run"

// Run states. A run that started reports exited (by itself) or timed_out
// (stopped at its deadline); termination holds the leader's own status.
const (
	RunStateExited     = "exited"
	RunStateTimedOut   = "timed_out"
	RunStateNotStarted = "not_started"
)

// RunControls is the part of Controls that serves mesh_run.
type RunControls interface {
	Run(ctx context.Context, req RunRequest) (CallOutcome, error)
}

// RunRequest is one parsed mesh_run call.
type RunRequest struct {
	Context     ContextRef
	Command     string
	Argv        []string
	Cwd         string
	Env         map[string]string
	TimeoutMS   int
	OutputBytes int
}

// RunStreamData is one stream of a mesh_run result: its first bytes and how
// many the command wrote in all.
type RunStreamData struct {
	Encoding    string `json:"encoding"`
	Data        string `json:"data"`
	TotalBytes  int64  `json:"totalBytes"`
	Truncated   bool   `json:"truncated"`
	CloseReason string `json:"closeReason"`
}

// RunResultData is the structured result of mesh_run. It has no job handle:
// nothing about the run is kept after this reply.
type RunResultData struct {
	Target      TargetMeta                    `json:"target"`
	State       string                        `json:"state"`
	Termination agentapi.ExecutionTermination `json:"termination"`
	DurationMS  int64                         `json:"durationMs"`
	Stdout      RunStreamData                 `json:"stdout"`
	Stderr      RunStreamData                 `json:"stderr"`
}

// BuildRunResult renders the daemon's run reply for the agent.
func BuildRunResult(result *protocol.ExecutionRunResult, target TargetMeta) RunResultData {
	t := result.Termination
	data := RunResultData{
		Target:      target,
		State:       RunStateExited,
		Termination: agentapi.ExecutionTermination{Kind: t.Kind, Code: t.Code, Signal: t.Signal, Reason: t.Reason},
		DurationMS:  result.DurationMillis,
		Stdout:      buildRunStream(result.Stdout),
		Stderr:      buildRunStream(result.Stderr),
	}
	if t.Kind == RunStateNotStarted {
		data.State = RunStateNotStarted
	} else if result.TimedOut {
		data.State = RunStateTimedOut
	}
	return data
}

func buildRunStream(s protocol.ExecutionRunStream) RunStreamData {
	encoding, encoded := EncodeOutputBytes(s.Bytes)
	return RunStreamData{Encoding: encoding, Data: encoded, TotalBytes: s.Total, Truncated: s.Truncated, CloseReason: s.CloseReason}
}

func handleRun(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	req, err := parseRunArgs(raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.Run(ctx, req)
	return wrapControl("mesh_run", out, err)
}

func parseRunArgs(raw json.RawMessage) (RunRequest, error) {
	var args struct {
		Context     string            `json:"context"`
		Command     *string           `json:"command"`
		Argv        *[]string         `json:"argv"`
		Cwd         string            `json:"cwd"`
		Env         map[string]string `json:"env"`
		TimeoutMS   *int              `json:"timeout_ms"`
		OutputBytes *int              `json:"output_bytes"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return RunRequest{}, err
	}
	ref, err := ParseContextHandle(args.Context)
	if err != nil {
		return RunRequest{}, invalidParam("%v", err)
	}
	command, argv, err := parseExecCommand(args.Command, args.Argv)
	if err != nil {
		return RunRequest{}, err
	}
	if err := parseEnvMap(args.Env, "env"); err != nil {
		return RunRequest{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultWaitMS, maxTimeoutMS)
	if err != nil {
		return RunRequest{}, err
	}
	outputBytes, err := parseExecOutputBytes(args.OutputBytes)
	if err != nil {
		return RunRequest{}, err
	}
	req := RunRequest{Context: ref, Command: command, Argv: argv, Cwd: args.Cwd, Env: args.Env, TimeoutMS: int(timeout.Milliseconds()), OutputBytes: outputBytes}
	return req, validateRunEncodedSize(req)
}

// RunControl is the execution.run control for req, without caller or
// request ID.
func RunControl(req RunRequest) protocol.Control {
	return protocol.Control{
		Type:          protocol.TypeExecutionRun,
		Command:       req.Argv,
		Cwd:           req.Cwd,
		TimeoutMillis: req.TimeoutMS,
		OutputBytes:   req.OutputBytes,
		ExecutionContext: &protocol.ExecutionContextRequest{
			ContextID:    req.Context.ContextID,
			ShellCommand: req.Command,
			Env:          req.Env,
		},
	}
}

// validateRunEncodedSize checks the control fits Mesh's execution limits
// before anything is sent, with room for the caller and request ID.
func validateRunEncodedSize(req RunRequest) error {
	probe := RunControl(req)
	probe.RequestID = "00000000000000000000000000000000"
	probe.Caller = protocol.CallerCredential("mac1_0000000000000000000000000000000000000000000000000000000000000000")
	encoded, err := probe.Encode()
	if err != nil {
		return invalidParam("run request cannot be encoded: %v", err)
	}
	if _, err := protocol.DecodeControl(encoded); err != nil {
		return invalidParam("run request exceeds Mesh control limits or failed validation: %v", err)
	}
	return nil
}

var runHints = map[agentapi.Code]string{
	agentapi.RequestOutcomeUnknown: "mesh_run keeps no receipt, so the command may or may not have run: check its effects before running it again, and use mesh_exec with a request_key when you must know.",
	agentapi.ResourceLimit:         "Too many runs at once for this host; wait for one to finish or retry shortly.",
	agentapi.HostUnreachable:       "Nothing ran. Check that the host is online or call mesh_host_wake.",
	agentapi.ContextClosed:         "Open a new context with mesh_context_open.",
}

func renderRunResult(b *strings.Builder, data RunResultData) {
	fmt.Fprintf(b, "run: %s", data.State)
	renderTermination(b, data.Termination)
	fmt.Fprintf(b, " (%d ms)", data.DurationMS)
	renderRunStream(b, "stdout", data.Stdout)
	renderRunStream(b, "stderr", data.Stderr)
}

func renderRunStream(b *strings.Builder, name string, s RunStreamData) {
	if s.Data == "" && !s.Truncated && s.CloseReason == "eof" {
		return
	}
	fmt.Fprintf(b, "\n%s (%d bytes", name, s.TotalBytes)
	if s.Truncated {
		b.WriteString(", truncated")
	}
	if s.CloseReason != "" && s.CloseReason != "eof" {
		b.WriteString(", " + s.CloseReason)
	}
	b.WriteString("):\n")
	if s.Encoding == encodingUTF8 {
		b.WriteString(escapeControlText(s.Data))
		return
	}
	b.WriteString(s.Data)
}
