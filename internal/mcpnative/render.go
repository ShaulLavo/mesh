package mcpnative

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/shaul/mesh/internal/agentapi"
)

const maxRenderedTextBytes = 64 << 10

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
		if hint := nextActionHint(item.Code); hint != "" {
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
}

func nextActionHint(code agentapi.Code) string {
	return nextActionHints[code]
}

func renderErrorRecoveryData(b *strings.Builder, data any) {
	switch v := data.(type) {
	case JobOutputData:
		if v.EarliestCursor != "" {
			fmt.Fprintf(b, "\nearliestCursor: %s", v.EarliestCursor)
		}
	case *JobOutputData:
		if v != nil && v.EarliestCursor != "" {
			fmt.Fprintf(b, "\nearliestCursor: %s", v.EarliestCursor)
		}
	case JobStateData:
		if v.Job != "" {
			fmt.Fprintf(b, "\njob: %s (%s)", v.Job, v.State)
		}
	case *JobStateData:
		if v != nil && v.Job != "" {
			fmt.Fprintf(b, "\njob: %s (%s)", v.Job, v.State)
		}
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
	if data.Encoding == "utf-8" {
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
