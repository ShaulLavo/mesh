package mcpnative

import (
	"context"
	"encoding/json"
	"time"
)

const (
	maxTerminalDimension = 2048
	maxTerminalCells     = 256 << 10
	maxScreenCols        = 160
	maxScreenRows        = 24
	defaultTerminalTerm  = "xterm-256color"
)

func handleTerminalOpen(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	req, err := parseTerminalOpenArgs(raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.TerminalOpen(ctx, req)
	return wrapControl("mesh_terminal_open", out, err)
}

func parseTerminalOpenArgs(raw json.RawMessage) (TerminalOpenRequest, error) {
	var args struct {
		Context    string            `json:"context"`
		RequestKey string            `json:"request_key"`
		Command    *string           `json:"command"`
		Argv       []string          `json:"argv"`
		Cwd        string            `json:"cwd"`
		Env        map[string]string `json:"env"`
		Cols       *int              `json:"cols"`
		Rows       *int              `json:"rows"`
		Term       string            `json:"term"`
		TimeoutMS  *int              `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return TerminalOpenRequest{}, err
	}
	ref, err := ParseContextHandle(args.Context)
	if err != nil {
		return TerminalOpenRequest{}, invalidParam("%v", err)
	}
	if err := validateRequestKey(args.RequestKey); err != nil {
		return TerminalOpenRequest{}, err
	}
	command, argv, err := parseExecCommand(args.Command, args.Argv)
	if err != nil {
		return TerminalOpenRequest{}, err
	}
	if err := parseEnvMap(args.Env, "env"); err != nil {
		return TerminalOpenRequest{}, err
	}
	cols, rows, err := parseTerminalSize(args.Cols, args.Rows, 80, 24)
	if err != nil {
		return TerminalOpenRequest{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return TerminalOpenRequest{}, err
	}
	term := args.Term
	if term == "" {
		term = defaultTerminalTerm
	}
	return TerminalOpenRequest{
		Context: ref, RequestKey: args.RequestKey, Command: command, Argv: argv, Cwd: args.Cwd, Env: args.Env,
		Cols: cols, Rows: rows, Term: term, Timeout: timeout,
	}, nil
}

// parseTerminalSize applies defaults and the worker's size limits, so a bad
// size fails before anything reaches a host.
func parseTerminalSize(cols, rows *int, defaultCols, defaultRows int) (int, int, error) {
	c, r := defaultCols, defaultRows
	if cols != nil {
		c = *cols
	}
	if rows != nil {
		r = *rows
	}
	if c < 1 || r < 1 || c > maxTerminalDimension || r > maxTerminalDimension || c > maxTerminalCells/r {
		return 0, 0, invalidParam("cols and rows must be 1 to %d with at most %d cells", maxTerminalDimension, maxTerminalCells)
	}
	return c, r, nil
}

// parseJobTarget reads the job handle and its optional context handle, which
// must name the same host.
func parseJobTarget(job, contextHandle string) (JobRef, *ContextRef, error) {
	ref, err := ParseJobHandle(job)
	if err != nil {
		return JobRef{}, nil, invalidParam("%v", err)
	}
	ctxRef, err := parseOptionalContext(contextHandle)
	if err != nil {
		return JobRef{}, nil, err
	}
	if ctxRef != nil && ctxRef.HostID != ref.HostID {
		return JobRef{}, nil, invalidParam("context handle belongs to a different host than job handle")
	}
	return ref, ctxRef, nil
}

func handleTerminalInput(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Job        string  `json:"job"`
		Context    string  `json:"context"`
		WriteKey   string  `json:"write_key"`
		Offset     *string `json:"offset"`
		Data       *string `json:"data"`
		DataBase64 *string `json:"data_base64"`
		TimeoutMS  *int    `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ref, ctxRef, err := parseJobTarget(args.Job, args.Context)
	if err != nil {
		return CallOutcome{}, err
	}
	if err := validateWriteKey(args.WriteKey); err != nil {
		return CallOutcome{}, err
	}
	offset, data, err := parseInputPayload(args.Offset, args.Data, args.DataBase64, false)
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobInput(ctx, JobInputRequest{
		Job: ref, Context: ctxRef, WriteKey: args.WriteKey, Offset: offset, Data: data, Kind: jobKindTerminal, Timeout: timeout,
	})
	return wrapControl("mesh_terminal_input", out, err)
}

type terminalSizeArgs struct {
	Job       string `json:"job"`
	Context   string `json:"context"`
	Cols      *int   `json:"cols"`
	Rows      *int   `json:"rows"`
	TimeoutMS *int   `json:"timeout_ms"`
}

func parseTerminalSizeCall(raw json.RawMessage, required bool, defaultCols, defaultRows int) (JobRef, *ContextRef, int, int, time.Duration, error) {
	var args terminalSizeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return JobRef{}, nil, 0, 0, 0, err
	}
	ref, ctxRef, err := parseJobTarget(args.Job, args.Context)
	if err != nil {
		return JobRef{}, nil, 0, 0, 0, err
	}
	if required && (args.Cols == nil || args.Rows == nil) {
		return JobRef{}, nil, 0, 0, 0, invalidParam("cols and rows are required")
	}
	cols, rows, err := parseTerminalSize(args.Cols, args.Rows, defaultCols, defaultRows)
	if err != nil {
		return JobRef{}, nil, 0, 0, 0, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	return ref, ctxRef, cols, rows, timeout, err
}

func handleTerminalResize(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	ref, ctxRef, cols, rows, timeout, err := parseTerminalSizeCall(raw, true, 0, 0)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.TerminalResize(ctx, TerminalResizeRequest{Job: ref, Context: ctxRef, Cols: cols, Rows: rows, Timeout: timeout})
	return wrapControl("mesh_terminal_resize", out, err)
}

func handleTerminalScreen(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	ref, ctxRef, cols, rows, timeout, err := parseTerminalSizeCall(raw, false, 80, 24)
	if err != nil {
		return CallOutcome{}, err
	}
	if cols > maxScreenCols || rows > maxScreenRows {
		return CallOutcome{}, invalidParam("screen reads are at most %d by %d", maxScreenCols, maxScreenRows)
	}
	out, err := controls.TerminalScreen(ctx, TerminalScreenRequest{Job: ref, Context: ctxRef, Cols: cols, Rows: rows, Timeout: timeout})
	return wrapControl("mesh_terminal_screen", out, err)
}
