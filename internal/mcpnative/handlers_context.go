package mcpnative

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

func handleContextOpen(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host       string            `json:"host"`
		RequestKey string            `json:"request_key"`
		Cwd        string            `json:"cwd"`
		Shell      string            `json:"shell"`
		Env        map[string]string `json:"env"`
		Label      string            `json:"label"`
		TimeoutMS  *int              `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return CallOutcome{}, err
	}
	if err := validateRequestKey(args.RequestKey); err != nil {
		return CallOutcome{}, err
	}
	cwd, err := requireNonEmpty(args.Cwd, "cwd")
	if err != nil {
		return CallOutcome{}, err
	}
	shell, err := parseContextShell(args.Shell)
	if err != nil {
		return CallOutcome{}, err
	}
	if err := parseEnvMap(args.Env, "env"); err != nil {
		return CallOutcome{}, err
	}
	if len(args.Label) > protocol.MaxContextLabelBytes {
		return CallOutcome{}, invalidParam("label exceeds %d bytes", protocol.MaxContextLabelBytes)
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.OpenContext(ctx, OpenContextRequest{
		Host: host, RequestKey: args.RequestKey, Cwd: cwd, Shell: shell, Env: args.Env, Label: args.Label, Timeout: timeout,
	})
	return wrapControl("mesh_context_open", out, err)
}

func handleContextGet(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Context   string `json:"context"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ref, err := ParseContextHandle(args.Context)
	if err != nil {
		return CallOutcome{}, invalidParam("%v", err)
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.GetContext(ctx, ContextRefRequest{Context: ref, Timeout: timeout})
	return wrapControl("mesh_context_get", out, err)
}

func handleContextList(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	host, timeout, err := parseHostTimeoutCall(controls, raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.ListContexts(ctx, ListContextsRequest{Host: host, Timeout: timeout})
	return wrapControl("mesh_context_list", out, err)
}

func handleContextReceipt(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host       string `json:"host"`
		RequestKey string `json:"request_key"`
		TimeoutMS  *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return CallOutcome{}, err
	}
	if err := validateRequestKey(args.RequestKey); err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.ContextReceipt(ctx, ContextReceiptRequest{Host: host, RequestKey: args.RequestKey, Timeout: timeout})
	return wrapControl("mesh_context_receipt", out, err)
}

func handleContextUpdate(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Context          string            `json:"context"`
		ExpectedRevision json.RawMessage   `json:"expected_revision"`
		Cwd              string            `json:"cwd"`
		SetEnv           map[string]string `json:"set_env"`
		UnsetEnv         []string          `json:"unset_env"`
		Label            *string           `json:"label"`
		TimeoutMS        *int              `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ref, err := ParseContextHandle(args.Context)
	if err != nil {
		return CallOutcome{}, invalidParam("%v", err)
	}
	rev, err := parseRevision(args.ExpectedRevision, true)
	if err != nil {
		return CallOutcome{}, err
	}
	if err := validateContextUpdateChange(args.SetEnv, args.UnsetEnv, args.Label); err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.UpdateContext(ctx, UpdateContextRequest{
		Context: ref, ExpectedRevision: *rev, Cwd: args.Cwd, SetEnv: args.SetEnv, UnsetEnv: args.UnsetEnv, Label: args.Label, Timeout: timeout,
	})
	return wrapControl("mesh_context_update", out, err)
}

func validateContextUpdateChange(setEnv map[string]string, unsetEnv []string, label *string) error {
	if err := parseEnvMap(setEnv, "set_env"); err != nil {
		return err
	}
	for _, name := range unsetEnv {
		if err := validateEnvName(name, "unset_env"); err != nil {
			return err
		}
	}
	if label != nil && len(*label) > protocol.MaxContextLabelBytes {
		return invalidParam("label exceeds %d bytes", protocol.MaxContextLabelBytes)
	}
	return nil
}

func handleContextClose(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Context          string          `json:"context"`
		ExpectedRevision json.RawMessage `json:"expected_revision"`
		TimeoutMS        *int            `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ref, err := ParseContextHandle(args.Context)
	if err != nil {
		return CallOutcome{}, invalidParam("%v", err)
	}
	rev, err := parseRevision(args.ExpectedRevision, false)
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.CloseContext(ctx, CloseContextRequest{Context: ref, ExpectedRevision: rev, Timeout: timeout})
	return wrapControl("mesh_context_close", out, err)
}

func handleExec(ctx context.Context, controls Controls, raw json.RawMessage, notify func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	req, err := parseExecArgs(raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.Exec(ctx, req, notify)
	return wrapControl("mesh_exec", out, err)
}

func parseExecArgs(raw json.RawMessage) (ExecRequest, error) {
	var args struct {
		Context     string            `json:"context"`
		RequestKey  string            `json:"request_key"`
		Command     *string           `json:"command"`
		Argv        []string          `json:"argv"`
		Cwd         string            `json:"cwd"`
		Env         map[string]string `json:"env"`
		Stdin       string            `json:"stdin"`
		WaitMS      *int              `json:"wait_ms"`
		OutputBytes *int              `json:"output_bytes"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return ExecRequest{}, err
	}
	ref, err := ParseContextHandle(args.Context)
	if err != nil {
		return ExecRequest{}, invalidParam("%v", err)
	}
	if err := validateRequestKey(args.RequestKey); err != nil {
		return ExecRequest{}, err
	}
	cmd, argv, err := parseExecCommand(args.Command, args.Argv)
	if err != nil {
		return ExecRequest{}, err
	}
	if err := parseEnvMap(args.Env, "env"); err != nil {
		return ExecRequest{}, err
	}
	stdin, err := parseExecStdin(args.Stdin)
	if err != nil {
		return ExecRequest{}, err
	}
	waitMS, err := parseExecWaitMS(args.WaitMS)
	if err != nil {
		return ExecRequest{}, err
	}
	outputBytes, err := parseExecOutputBytes(args.OutputBytes)
	if err != nil {
		return ExecRequest{}, err
	}
	return ExecRequest{
		Context: ref, RequestKey: args.RequestKey, Command: cmd, Argv: argv,
		Cwd: args.Cwd, Env: args.Env, Stdin: stdin, WaitMS: waitMS, OutputBytes: outputBytes,
	}, nil
}

// BuildExecStreamData formats one initial output page for mesh_exec results.
func BuildExecStreamData(page *agentapi.ExecutionPage) *ExecStreamData {
	if page == nil {
		return nil
	}
	encoding, encoded := EncodeOutputBytes(page.Bytes)
	start, end := pageRangeStrings(page)
	if start == "" {
		start = "0"
		end = strconv.Itoa(len(page.Bytes))
	}
	var gap *ExecStreamGap
	if page.Gap {
		gap = &ExecStreamGap{Reason: page.Reason, EarliestCursor: page.EarliestCursor}
	}
	return &ExecStreamData{
		Encoding:   encoding,
		Data:       encoded,
		Start:      start,
		End:        end,
		NextCursor: page.NextCursor,
		EOF:        page.EOF,
		Gap:        gap,
	}
}
