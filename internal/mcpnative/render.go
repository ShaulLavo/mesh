package mcpnative

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/shaul/mesh/internal/agentapi"
)

const (
	maxRenderedTextBytes = 64 << 10
	encodingUTF8         = "utf-8"
)

func renderOutcomeText(envelope agentapi.Envelope, isError bool) string {
	var b strings.Builder
	if isError {
		renderErrors(&b, envelope.Errors)
		renderErrorRecoveryData(&b, envelope.Data)
		return truncateText(strings.TrimSpace(b.String()))
	}
	renderSuccessData(&b, envelope.Data)
	if len(envelope.Errors) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\nDiagnostics:\n")
		}
		renderErrors(&b, envelope.Errors)
	}
	if b.Len() == 0 {
		fmt.Fprintf(&b, "%s: %s", envelope.Operation, envelope.Status)
	}
	return truncateText(strings.TrimSpace(b.String()))
}

func renderErrors(b *strings.Builder, errs []agentapi.Error) {
	for i, item := range errs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(b, "[%s] %s", item.Code, escapeControlText(item.Message))
		if hint := nextActionHint(item.Operation, item.Code); hint != "" {
			b.WriteString(" Next: " + hint)
		}
	}
}

var nextActionHints = map[agentapi.Code]string{
	agentapi.HostUnreachable:          "Check that the host is online or call mesh_host_wake.",
	agentapi.HostIdentityMismatch:     "Verify the host identity and re-enroll before retrying.",
	agentapi.HostAuthenticationFailed: "Check device approval on the target host.",
	agentapi.AccessDenied:             "Check device approval on the target host.",
	agentapi.ProtocolUnsupported:      "Check host capabilities from mesh_hosts_list and update the target daemon.",
	agentapi.LogsCursorExpired:        "Resume reading from earliestCursor in structuredContent.data.",
	agentapi.LogsCursorInvalid:        "Use a cursor returned by mesh_job_output for this stream.",
	agentapi.ResultExpired:            "Output expired; use mesh_job_status or mesh_job_receipt for the retained exit details.",
	agentapi.RequestOutcomeUnknown:    "Query the saved request_key with mesh_job_receipt; do not replay blindly.",
	agentapi.InputOutcomeUnknown:      "Call mesh_job_input with the same write_key and no offset to recover its receipt.",
	agentapi.InputBackpressure:        "Retry mesh_job_input with the same write_key and offset after the job drains stdin.",
	agentapi.InputOffsetMismatch:      "Check the last acknowledged offset before sending the next write.",
	agentapi.RequestKeyConflict:       "Choose a new request_key for a different command or context intent.",
	agentapi.ContextNotFound:          "Verify the context handle and active --identity, or call mesh_context_list.",
	agentapi.ContextClosed:            "Open a new context with mesh_context_open; existing job handles remain readable.",
	agentapi.ContextConflict:          "Read the current revision with mesh_context_get and retry mesh_context_update.",
	agentapi.ResourceLimit:            "Close unused contexts with mesh_context_close before opening another.",
}

// nextActionHint picks the guidance for a failed operation. File and profile
// operations have their own, because the job and context guidance names tools
// that do not apply to them; every other operation keeps the shared hints.
func nextActionHint(operation string, code agentapi.Code) string {
	if hint := operationHint(operation, code); hint != "" {
		return hint
	}
	return nextActionHints[code]
}

func renderErrorRecoveryData(b *strings.Builder, data any) {
	switch v := data.(type) {
	case JobOutputData:
		renderOutputCursorHint(b, v.EarliestCursor)
	case *JobOutputData:
		if v != nil {
			renderOutputCursorHint(b, v.EarliestCursor)
		}
	case JobStateData:
		renderJobRecoveryHint(b, v.Job, v.State)
	case *JobStateData:
		if v != nil {
			renderJobRecoveryHint(b, v.Job, v.State)
		}
	case agentapi.ExecutionContext:
		renderContextRecoveryHint(b, v)
	case *agentapi.ExecutionContext:
		if v != nil {
			renderContextRecoveryHint(b, *v)
		}
	case ExecResultData:
		renderJobRecoveryHint(b, v.Job, v.State)
	case *ExecResultData:
		if v != nil {
			renderJobRecoveryHint(b, v.Job, v.State)
		}
	}
}

func renderOutputCursorHint(b *strings.Builder, earliest string) {
	if earliest != "" {
		fmt.Fprintf(b, "\nearliestCursor: %s", earliest)
	}
}

func renderJobRecoveryHint(b *strings.Builder, job, state string) {
	if job != "" {
		fmt.Fprintf(b, "\njob: %s (%s)", job, state)
	}
}

func renderContextRecoveryHint(b *strings.Builder, c agentapi.ExecutionContext) {
	if c.Handle != "" {
		fmt.Fprintf(b, "\ncontext: %s (state=%s revision=%d)", c.Handle, c.State, c.Revision)
	}
}

func renderSuccessData(b *strings.Builder, data any) {
	switch v := data.(type) {
	case SkillData:
		b.WriteString(v.Content)
	case agentapi.HostsData:
		renderHostsData(b, v)
	case agentapi.HostDetail:
		renderHostDetail(b, v)
	case WakeResult:
		renderWakeResult(b, v)
	case agentapi.SessionPage:
		renderSessionPage(b, v)
	case agentapi.SessionDetail:
		renderSessionDetail(b, v)
	case agentapi.ServicesData:
		renderServicesData(b, v)
	case ServiceDetail:
		renderServiceRow(b, v.Service)
	case agentapi.ExecutionContext:
		renderExecutionContext(b, v)
	case *agentapi.ExecutionContext:
		if v != nil {
			renderExecutionContext(b, *v)
		}
	case agentapi.ExecutionContexts:
		renderExecutionContexts(b, v)
	case ExecResultData:
		renderExecResult(b, v)
	case *ExecResultData:
		if v != nil {
			renderExecResult(b, *v)
		}
	case JobStateData:
		renderJobState(b, v)
	case *JobStateData:
		if v != nil {
			renderJobState(b, *v)
		}
	case JobOutputData:
		renderJobOutput(b, v)
	case *JobOutputData:
		if v != nil {
			renderJobOutput(b, *v)
		}
	case JobListData:
		renderJobList(b, v)
	case JobInputData:
		fmt.Fprintf(b, "job %s stdin write %s offset=%s length=%d closed=%t", v.Job, v.WriteKey, v.Offset, v.Length, v.Closed)
	default:
		renderFileData(b, data)
		renderTerminalAndServiceData(b, data)
	}
}

func renderHostsData(b *strings.Builder, data agentapi.HostsData) {
	if len(data.Hosts) == 0 {
		b.WriteString("No hosts configured.")
		return
	}
	for i, h := range data.Hosts {
		if i > 0 {
			b.WriteByte('\n')
		}
		id := "uninitialized"
		if h.HostID != nil {
			id = *h.HostID
		}
		name := h.MachineName
		if name == "" {
			name = id
		}
		fmt.Fprintf(b, "%s (%s): %s", escapeControlText(name), id, h.Reachability)
		if h.Capabilities != nil {
			fmt.Fprintf(b, " [pipe=%t context=%t files=%t mcp=%t]", h.Capabilities.ExecutionPipe, h.Capabilities.ExecutionContext, h.Capabilities.Files, h.Capabilities.MCPHosting)
		}
	}
}

func renderHostDetail(b *strings.Builder, detail agentapi.HostDetail) {
	renderHostsData(b, agentapi.HostsData{Hosts: []agentapi.HostSummary{detail.Host}})
	fmt.Fprintf(b, "\naccess: %s (%s, %s)", detail.Access.State, detail.Access.Grant, detail.Access.Transport)
}

func renderWakeResult(b *strings.Builder, r WakeResult) {
	label := r.MachineName
	if label == "" {
		label = r.HostID
	}
	if r.AlreadyOnline {
		fmt.Fprintf(b, "%s (%s) is already online.", escapeControlText(label), r.HostID)
		return
	}
	fmt.Fprintf(b, "Wake packet for %s (%s) sent by %s.", escapeControlText(label), r.HostID, escapeControlText(r.Sender))
}

func renderSessionPage(b *strings.Builder, page agentapi.SessionPage) {
	if len(page.Sessions) == 0 {
		fmt.Fprintf(b, "No sessions on %s (%s).", page.HostID, page.Freshness)
		return
	}
	for i, s := range page.Sessions {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(b, "%s %s (%s)", s.ID, s.State, s.Kind)
		if s.Label != "" {
			fmt.Fprintf(b, " %s", escapeControlText(s.Label))
		}
	}
}

func renderSessionDetail(b *strings.Builder, detail agentapi.SessionDetail) {
	fmt.Fprintf(b, "session %s on %s: %s (liveState=%s, freshness=%s)", detail.Session.ID, detail.Session.HostID, detail.Session.State, detail.LiveState, detail.Freshness)
	if detail.Live != nil && len(detail.Live.Preview) > 0 {
		b.WriteString("\npreview:\n")
		for _, line := range detail.Live.Preview {
			b.WriteString(escapeControlText(line) + "\n")
		}
	}
}

func renderServicesData(b *strings.Builder, data agentapi.ServicesData) {
	if len(data.Services) == 0 {
		b.WriteString("No services.")
		return
	}
	for i, s := range data.Services {
		if i > 0 {
			b.WriteByte('\n')
		}
		renderServiceRow(b, s)
	}
}

func renderServiceRow(b *strings.Builder, s agentapi.ServiceRow) {
	fmt.Fprintf(b, "%s/%s (%s, %s, %s)", escapeControlText(s.MachineName), escapeControlText(s.Name), s.Kind, s.Scope, s.Health)
	if s.Problem != "" {
		fmt.Fprintf(b, ": %s", escapeControlText(s.Problem))
	}
}

func renderJobState(b *strings.Builder, data JobStateData) {
	fmt.Fprintf(b, "job %s: %s", data.Job, data.State)
	if data.Wait != "" {
		fmt.Fprintf(b, " (wait=%s)", data.Wait)
	}
	if data.Delivery != "" {
		fmt.Fprintf(b, " (delivery=%s)", data.Delivery)
	}
	if data.Result != nil {
		renderTermination(b, data.Result.Termination)
	}
}

func renderTermination(b *strings.Builder, t agentapi.ExecutionTermination) {
	fmt.Fprintf(b, " termination=%s", t.Kind)
	if t.Code != nil {
		fmt.Fprintf(b, " code=%d", *t.Code)
	}
	if t.Signal != nil {
		fmt.Fprintf(b, " signal=%d", *t.Signal)
	}
	if t.Reason != "" {
		fmt.Fprintf(b, " reason=%s", escapeControlText(t.Reason))
	}
}

func renderJobOutput(b *strings.Builder, data JobOutputData) {
	fmt.Fprintf(b, "job %s %s [%s..%s] eof=%t encoding=%s", data.Job, data.Stream, data.Start, data.End, data.EOF, data.Encoding)
	if data.Data == "" {
		return
	}
	b.WriteByte('\n')
	if data.Encoding == encodingUTF8 {
		b.WriteString(escapeControlText(data.Data))
		return
	}
	b.WriteString(data.Data)
}

func renderJobList(b *strings.Builder, data JobListData) {
	if len(data.Jobs) == 0 {
		fmt.Fprintf(b, "No jobs on %s.", data.HostID)
		return
	}
	for i, item := range data.Jobs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(b, "%s %s", item.Job, item.State)
	}
}

func renderExecutionContext(b *strings.Builder, c agentapi.ExecutionContext) {
	fmt.Fprintf(b, "context %s: %s rev=%d cwd=%s shell=%s runningJobs=%d", c.Handle, c.State, c.Revision, escapeControlText(c.Cwd), c.Shell, c.RunningJobs)
	if c.UncertainJobs > 0 {
		fmt.Fprintf(b, " uncertainJobs=%d", c.UncertainJobs)
	}
	if c.Label != "" {
		fmt.Fprintf(b, " label=%s", escapeControlText(c.Label))
	}
	if len(c.EnvNames) > 0 {
		fmt.Fprintf(b, " env=[%s]", strings.Join(c.EnvNames, ","))
	}
}

func renderExecutionContexts(b *strings.Builder, list agentapi.ExecutionContexts) {
	if len(list.Contexts) == 0 {
		fmt.Fprintf(b, "No contexts on %s.", list.HostID)
		return
	}
	for i, c := range list.Contexts {
		if i > 0 {
			b.WriteByte('\n')
		}
		renderExecutionContext(b, c)
	}
}

func renderExecResult(b *strings.Builder, data ExecResultData) {
	fmt.Fprintf(b, "job %s: %s", data.Job, data.State)
	if data.Wait != "" {
		fmt.Fprintf(b, " (wait=%s)", data.Wait)
	}
	if data.Result != nil {
		renderTermination(b, data.Result.Termination)
	}
	renderExecStream(b, "stdout", data.Stdout)
	renderExecStream(b, "stderr", data.Stderr)
}

func renderExecStream(b *strings.Builder, name string, stream *ExecStreamData) {
	if stream == nil || stream.Data == "" {
		return
	}
	fmt.Fprintf(b, "\n%s [%s..%s]:\n", name, stream.Start, stream.End)
	if stream.Encoding == encodingUTF8 {
		b.WriteString(escapeControlText(stream.Data))
		return
	}
	b.WriteString(stream.Data)
}

// escapeControlText escapes terminal control characters while keeping newlines and tabs.
func escapeControlText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "\\u%04x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func truncateText(text string) string {
	if len(text) <= maxRenderedTextBytes {
		return text
	}
	const suffix = "\n… (truncated; read structuredContent for complete page)"
	limit := maxRenderedTextBytes - len(suffix)
	for limit > 0 && (text[limit]&0xc0) == 0x80 {
		limit--
	}
	return text[:limit] + suffix
}
