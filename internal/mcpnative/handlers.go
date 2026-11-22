package mcpnative

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/skills"
)

func requireControls(controls Controls) error {
	if controls == nil {
		return errors.New("native controls are not configured")
	}
	return nil
}

func wrapControl(tool string, out CallOutcome, err error) (CallOutcome, error) {
	if err != nil {
		return out, fmt.Errorf("%s: %w", tool, err)
	}
	return out, nil
}

func handleSkill(_ context.Context, _ Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	var args struct {
		File string `json:"file"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	name := args.File
	if name == "" {
		name = "SKILL.md"
	}
	data, err := skills.Show(name)
	if err != nil {
		return CallOutcome{
			Envelope: errorEnvelope("skill.show", name, agentapi.UsageInvalid, err.Error()),
			IsError:  true,
		}, nil
	}
	bundle := skills.Bundle()
	names := make([]string, len(bundle))
	for i, f := range bundle {
		names[i] = f.Path
	}
	return CallOutcome{
		Envelope: okEnvelope("skill.show", SkillData{
			File:    name,
			Digest:  skills.Digest(),
			Files:   names,
			Content: string(data),
		}),
	}, nil
}

func handleHostsList(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	var args struct {
		Cached    bool `json:"cached"`
		TimeoutMS *int `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultHostMS, maxHostListMS)
	if err != nil {
		return CallOutcome{}, err
	}
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.ListHosts(ctx, ListHostsRequest{Cached: args.Cached, Timeout: timeout})
	return wrapControl("mesh_hosts_list", out, err)
}

func handleHostInspect(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host      string `json:"host"`
		Cached    bool   `json:"cached"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.InspectHost(ctx, InspectHostRequest{Host: host, Cached: args.Cached, Timeout: timeout})
	return wrapControl("mesh_host_inspect", out, err)
}

func parseHostTimeoutCall(controls Controls, raw json.RawMessage) (string, time.Duration, error) {
	if err := requireControls(controls); err != nil {
		return "", 0, err
	}
	var args struct {
		Host      string `json:"host"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", 0, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return "", 0, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return "", 0, err
	}
	return host, timeout, nil
}

func handleHostWake(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	host, timeout, err := parseHostTimeoutCall(controls, raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.WakeHost(ctx, WakeHostRequest{Host: host, Timeout: timeout})
	return wrapControl("mesh_host_wake", out, err)
}

func handleSessionsList(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host      string `json:"host"`
		All       bool   `json:"all"`
		Detail    bool   `json:"detail"`
		Limit     *int   `json:"limit"`
		Page      string `json:"page"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return CallOutcome{}, err
	}
	limit, err := parseLimit(args.Limit)
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.ListSessions(ctx, ListSessionsRequest{
		Host: host, All: args.All, Detail: args.Detail, Limit: limit, Page: args.Page, Timeout: timeout,
	})
	return wrapControl("mesh_sessions_list", out, err)
}

func handleSessionInspect(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host      string `json:"host"`
		SessionID string `json:"session_id"`
		Preview   bool   `json:"preview"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return CallOutcome{}, err
	}
	parsed, err := session.ParseID(args.SessionID)
	if err != nil || parsed != args.SessionID {
		return CallOutcome{}, invalidParam("session_id must be a canonical Mesh session ID")
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.InspectSession(ctx, InspectSessionRequest{
		Host: host, SessionID: args.SessionID, Preview: args.Preview, Timeout: timeout,
	})
	return wrapControl("mesh_session_inspect", out, err)
}

func handleServicesList(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host      string `json:"host"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.ListServices(ctx, ListServicesRequest{Host: args.Host, Timeout: timeout})
	return wrapControl("mesh_services_list", out, err)
}

func handleServiceInspect(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host      string `json:"host"`
		Name      string `json:"name"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return CallOutcome{}, err
	}
	name, err := requireNonEmpty(args.Name, "name")
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.InspectService(ctx, InspectServiceRequest{Host: host, Name: name, Timeout: timeout})
	return wrapControl("mesh_service_inspect", out, err)
}

func parseJobRefArgs(raw json.RawMessage) (JobRefRequest, error) {
	var args struct {
		Job       string `json:"job"`
		Context   string `json:"context"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return JobRefRequest{}, err
	}
	ref, err := ParseJobHandle(args.Job)
	if err != nil {
		return JobRefRequest{}, invalidParam("%v", err)
	}
	ctxRef, err := parseOptionalContext(args.Context)
	if err != nil {
		return JobRefRequest{}, err
	}
	if ctxRef != nil && ctxRef.HostID != ref.HostID {
		return JobRefRequest{}, invalidParam("context handle belongs to a different host than job handle")
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return JobRefRequest{}, err
	}
	return JobRefRequest{Job: ref, Context: ctxRef, Timeout: timeout}, nil
}

func handleJobStatus(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	req, err := parseJobRefArgs(raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobStatus(ctx, req)
	return wrapControl("mesh_job_status", out, err)
}

func handleJobWait(ctx context.Context, controls Controls, raw json.RawMessage, notify func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Job     string `json:"job"`
		Context string `json:"context"`
		WaitMS  *int   `json:"wait_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ref, err := ParseJobHandle(args.Job)
	if err != nil {
		return CallOutcome{}, invalidParam("%v", err)
	}
	ctxRef, err := parseOptionalContext(args.Context)
	if err != nil {
		return CallOutcome{}, err
	}
	if ctxRef != nil && ctxRef.HostID != ref.HostID {
		return CallOutcome{}, invalidParam("context handle belongs to a different host than job handle")
	}
	waitMS, err := parseWaitMS(args.WaitMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobWait(ctx, JobWaitRequest{Job: ref, Context: ctxRef, WaitMS: waitMS}, notify)
	return wrapControl("mesh_job_wait", out, err)
}

func handleJobOutput(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Job       string `json:"job"`
		Context   string `json:"context"`
		Stream    string `json:"stream"`
		Cursor    string `json:"cursor"`
		MaxBytes  *int   `json:"max_bytes"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ref, err := ParseJobHandle(args.Job)
	if err != nil {
		return CallOutcome{}, invalidParam("%v", err)
	}
	ctxRef, err := parseOptionalContext(args.Context)
	if err != nil {
		return CallOutcome{}, err
	}
	if ctxRef != nil && ctxRef.HostID != ref.HostID {
		return CallOutcome{}, invalidParam("context handle belongs to a different host than job handle")
	}
	stream, err := parseOutputStream(args.Stream)
	if err != nil {
		return CallOutcome{}, err
	}
	if len(args.Cursor) > maxCursorLength {
		return CallOutcome{}, invalidParam("cursor exceeds %d bytes", maxCursorLength)
	}
	maxBytes, err := parseMaxBytes(args.MaxBytes)
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobOutput(ctx, JobOutputRequest{
		Job: ref, Context: ctxRef, Stream: stream, Cursor: args.Cursor, MaxBytes: maxBytes, Timeout: timeout,
	})
	return wrapControl("mesh_job_output", out, err)
}

func handleJobReceipt(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host       string `json:"host"`
		Context    string `json:"context"`
		RequestKey string `json:"request_key"`
		TimeoutMS  *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ctxRef, err := parseOptionalContext(args.Context)
	if err != nil {
		return CallOutcome{}, err
	}
	host := args.Host
	if ctxRef != nil {
		if host != "" && host != ctxRef.HostID {
			return CallOutcome{}, invalidParam("host names a different host than context handle")
		}
		host = ctxRef.HostID
	} else {
		host, err = requireNonEmpty(host, "host or context")
		if err != nil {
			return CallOutcome{}, err
		}
	}
	if err := validateRequestKey(args.RequestKey); err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobReceipt(ctx, JobReceiptRequest{Host: host, Context: ctxRef, RequestKey: args.RequestKey, Timeout: timeout})
	return wrapControl("mesh_job_receipt", out, err)
}

func handleJobsList(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host      string `json:"host"`
		Context   string `json:"context"`
		All       bool   `json:"all"`
		Limit     *int   `json:"limit"`
		Page      string `json:"page"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ctxRef, err := parseOptionalContext(args.Context)
	if err != nil {
		return CallOutcome{}, err
	}
	host := args.Host
	if ctxRef != nil {
		if host != "" && host != ctxRef.HostID {
			return CallOutcome{}, invalidParam("host names a different host than context handle")
		}
		host = ctxRef.HostID
	} else {
		host, err = requireNonEmpty(host, "host or context")
		if err != nil {
			return CallOutcome{}, err
		}
	}
	limit, err := parseLimit(args.Limit)
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.ListJobs(ctx, ListJobsRequest{
		Host: host, Context: ctxRef, All: args.All, Limit: limit, Page: args.Page, Timeout: timeout,
	})
	return wrapControl("mesh_jobs_list", out, err)
}

func handleJobInput(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
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
		EOF        bool    `json:"eof"`
		TimeoutMS  *int    `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ref, err := ParseJobHandle(args.Job)
	if err != nil {
		return CallOutcome{}, invalidParam("%v", err)
	}
	ctxRef, err := parseOptionalContext(args.Context)
	if err != nil {
		return CallOutcome{}, err
	}
	if ctxRef != nil && ctxRef.HostID != ref.HostID {
		return CallOutcome{}, invalidParam("context handle belongs to a different host than job handle")
	}
	if err := validateWriteKey(args.WriteKey); err != nil {
		return CallOutcome{}, err
	}
	offset, data, err := parseInputPayload(args.Offset, args.Data, args.DataBase64, args.EOF)
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobInput(ctx, JobInputRequest{
		Job: ref, Context: ctxRef, WriteKey: args.WriteKey, Offset: offset, Data: data, EOF: args.EOF, Timeout: timeout,
	})
	return wrapControl("mesh_job_input", out, err)
}

func handleJobSignal(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Job       string `json:"job"`
		Context   string `json:"context"`
		Signal    string `json:"signal"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	ref, err := ParseJobHandle(args.Job)
	if err != nil {
		return CallOutcome{}, invalidParam("%v", err)
	}
	ctxRef, err := parseOptionalContext(args.Context)
	if err != nil {
		return CallOutcome{}, err
	}
	if ctxRef != nil && ctxRef.HostID != ref.HostID {
		return CallOutcome{}, invalidParam("context handle belongs to a different host than job handle")
	}
	sig, err := validateSignalName(args.Signal)
	if err != nil {
		return CallOutcome{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobSignal(ctx, JobSignalRequest{Job: ref, Context: ctxRef, Signal: sig, Timeout: timeout})
	return wrapControl("mesh_job_signal", out, err)
}

func handleJobCancel(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	req, err := parseJobRefArgs(raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobCancel(ctx, req)
	return wrapControl("mesh_job_cancel", out, err)
}

func handleJobRemove(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	req, err := parseJobRefArgs(raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.JobRemove(ctx, req)
	return wrapControl("mesh_job_remove", out, err)
}

// EncodeOutputBytes returns utf-8 for valid NUL-free UTF-8, otherwise base64.
func EncodeOutputBytes(data []byte) (encoding, encoded string) {
	if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		return encodingUTF8, string(data)
	}
	return "base64", base64.StdEncoding.EncodeToString(data)
}

// BuildJobOutputData converts an agentapi.ExecutionPage into the MCP job output shape.
func BuildJobOutputData(page *agentapi.ExecutionPage, target TargetMeta) JobOutputData {
	ref := JobRef{HostID: page.HostID, SessionID: page.SessionID, Instance: page.Instance}
	encoding, encoded := EncodeOutputBytes(page.Bytes)
	start, end := pageRangeStrings(page)
	return JobOutputData{
		Job:            ref.String(),
		Target:         target,
		Stream:         page.Stream,
		Generation:     page.Generation,
		Encoding:       encoding,
		Data:           encoded,
		Start:          start,
		End:            end,
		StartCursor:    page.StartCursor,
		NextCursor:     page.NextCursor,
		EarliestCursor: page.EarliestCursor,
		EndCursor:      page.EndCursor,
		ObservedEnd:    page.ObservedEnd,
		StreamClosed:   page.StreamClosed,
		EOF:            page.EOF,
		CloseReason:    page.CloseReason,
		Loss:           page.Loss,
		Available:      page.Available,
		Gap:            page.Gap,
		Reason:         page.Reason,
	}
}

// JobStateRunning is the canonical MCP job state string for a live job.
const JobStateRunning = "running"

// JobStateFromReceipt maps an execution receipt's phase and termination to a concise job state.
func JobStateFromReceipt(receipt *agentapi.ExecutionReceipt) string {
	if receipt == nil {
		return "unknown"
	}
	if receipt.Phase == "live" {
		return JobStateRunning
	}
	if receipt.Phase == "terminal" && receipt.Result != nil && receipt.Result.Termination.Kind != "" {
		if receipt.Result.Termination.Kind == "exit" {
			return "exited"
		}
		return receipt.Result.Termination.Kind
	}
	return receipt.Phase
}

func pageRangeStrings(page *agentapi.ExecutionPage) (string, string) {
	start := cursorOffset(page.StartCursor)
	if start == "" {
		return "", ""
	}
	n, err := strconv.ParseUint(start, 10, 64)
	if err != nil {
		return start, ""
	}
	return start, strconv.FormatUint(n+uint64(len(page.Bytes)), 10)
}

func cursorOffset(token string) string {
	if token == "" {
		return ""
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return ""
	}
	var parsed struct {
		Offset string `json:"offset"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	return parsed.Offset
}

func okEnvelope(operation string, data any) agentapi.Envelope {
	id, _ := agentapi.NewRequestID()
	return agentapi.Envelope{
		SchemaVersion: agentapi.SchemaVersion,
		Operation:     operation,
		Status:        agentapi.StatusOK,
		RequestID:     id,
		Data:          data,
		Errors:        []agentapi.Error{},
	}
}

func errorEnvelope(operation, target string, code agentapi.Code, message string) agentapi.Envelope {
	id, _ := agentapi.NewRequestID()
	return agentapi.Envelope{
		SchemaVersion: agentapi.SchemaVersion,
		Operation:     operation,
		Status:        agentapi.StatusError,
		RequestID:     id,
		Errors:        []agentapi.Error{agentapi.Fail(code, operation, target, message).Err},
	}
}

func normalizeEnvelope(envelope agentapi.Envelope, fallbackOp string) agentapi.Envelope {
	if envelope.SchemaVersion == 0 {
		envelope.SchemaVersion = agentapi.SchemaVersion
	}
	if envelope.Operation == "" {
		envelope.Operation = fallbackOp
	}
	if envelope.RequestID == "" {
		envelope.RequestID, _ = agentapi.NewRequestID()
	}
	if envelope.Errors == nil {
		envelope.Errors = []agentapi.Error{}
	}
	for i := range envelope.Errors {
		envelope.Errors[i].Message = agentapi.CleanMessage(envelope.Errors[i].Message)
	}
	return envelope
}

func buildToolResult(outcome CallOutcome, fallbackOp string) (json.RawMessage, error) {
	envelope := normalizeEnvelope(outcome.Envelope, fallbackOp)
	isError := outcome.IsError || envelope.Status == agentapi.StatusError
	text := renderOutcomeText(envelope, isError)
	result := map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": envelope,
		"isError":           isError,
	}
	if outcome.Target != nil && outcome.Target.HostID != "" {
		result["_meta"] = map[string]any{MetaTargetKey: outcome.Target}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode MCP tool result: %w", err)
	}
	return raw, nil
}
