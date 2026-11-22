package mcpnative

import (
	"context"
	"encoding/json"
	"sort"
)

const (
	groupSkill    = "skill"
	groupHosts    = "hosts"
	groupSessions = "sessions"
	groupServices = "services"
	groupJobs     = "jobs"
)

// ToolAnnotations carries MCP tool behavioral hints.
type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// ToolHandler executes one native MCP tool call.
type ToolHandler func(ctx context.Context, controls Controls, args json.RawMessage, notify func(ProgressUpdate)) (CallOutcome, error)

// ToolSpec is the single source of truth for one native MCP tool.
type ToolSpec struct {
	Name        string
	Title       string
	Operation   string // Linked agentapi.Operation name when a CLI operation exists.
	Group       string
	Description string
	InputSchema json.RawMessage
	Annotations ToolAnnotations
	Handler     ToolHandler
}

// ToolDef is the MCP tools/list wire shape for one tool.
type ToolDef struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Operation   string          `json:"-"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations ToolAnnotations `json:"annotations"`
}

// Registry holds the deterministically ordered native MCP tool catalog.
type Registry struct {
	ordered []ToolSpec
	byName  map[string]ToolSpec
}

// DefaultRegistry builds the shipped native MCP tool registry sorted by tool name.
func DefaultRegistry() *Registry {
	specs := append(nativeToolSpecs(), fileToolSpecs()...)
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	byName := make(map[string]ToolSpec, len(specs))
	for _, spec := range specs {
		byName[spec.Name] = spec
	}
	return &Registry{ordered: specs, byName: byName}
}

// Definitions returns the wire tool definitions for tools/list.
func (r *Registry) Definitions() []ToolDef {
	defs := make([]ToolDef, len(r.ordered))
	for i, spec := range r.ordered {
		defs[i] = ToolDef{
			Name:        spec.Name,
			Title:       spec.Title,
			Operation:   spec.Operation,
			Description: spec.Description,
			InputSchema: spec.InputSchema,
			Annotations: spec.Annotations,
		}
	}
	return defs
}

// Lookup returns one tool specification by name.
func (r *Registry) Lookup(name string) (ToolSpec, bool) {
	spec, ok := r.byName[name]
	return spec, ok
}

func boolPtr(v bool) *bool { return &v }

func readOnlyAnnotations() ToolAnnotations {
	return ToolAnnotations{
		ReadOnlyHint:    boolPtr(true),
		DestructiveHint: boolPtr(false),
		IdempotentHint:  boolPtr(true),
		OpenWorldHint:   boolPtr(false),
	}
}

func idempotentWriteAnnotations() ToolAnnotations {
	return ToolAnnotations{
		ReadOnlyHint:    boolPtr(false),
		DestructiveHint: boolPtr(false),
		IdempotentHint:  boolPtr(true),
		OpenWorldHint:   boolPtr(false),
	}
}

func destructiveAnnotations(idempotent bool) ToolAnnotations {
	return ToolAnnotations{
		ReadOnlyHint:    boolPtr(false),
		DestructiveHint: boolPtr(true),
		IdempotentHint:  boolPtr(idempotent),
		OpenWorldHint:   boolPtr(false),
	}
}

func nativeToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        "mesh_skill",
			Title:       "Read embedded Mesh skill instructions",
			Operation:   "skill.show",
			Group:       groupSkill,
			Description: "Read Mesh's embedded agent skill instructions and reference documents without reading configuration or contacting any daemon.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"file":{"type":"string","description":"Embedded skill file path such as SKILL.md, references/commands.md, references/safety.md, or references/mcp-tools.md. Defaults to SKILL.md."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleSkill,
		},
		{
			Name:        "mesh_hosts_list",
			Title:       "List Mesh hosts, reachability, and capabilities",
			Operation:   "hosts.list",
			Group:       groupHosts,
			Description: "List configured Mesh hosts with reachability, freshness, protocol capability booleans, and summary host and fleet notes. Never wakes a host or starts a daemon.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"cached":{"type":"boolean","description":"Read local files and cached observations without dialing hosts."},"timeout_ms":{"type":"integer","minimum":100,"maximum":30000,"description":"Hard deadline in milliseconds (100 to 30000, default 2000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleHostsList,
		},
		{
			Name:        "mesh_host_inspect",
			Title:       "Inspect one Mesh host and its notes",
			Operation:   "hosts.inspect",
			Group:       groupHosts,
			Description: "Inspect one Mesh host for full owner notes, fleet notes, capabilities, and current access level. Never wakes the host or starts a daemon.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"cached":{"type":"boolean","description":"Read cached observations without dialing."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleHostInspect,
		},
		{
			Name:        "mesh_host_wake",
			Title:       "Wake a Mesh host",
			Operation:   "hosts.wake",
			Group:       groupHosts,
			Description: "Send a wake-on-LAN request for an approved Mesh host through an online peer. Idempotent when the host is already online.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: idempotentWriteAnnotations(),
			Handler:     handleHostWake,
		},
		{
			Name:        "mesh_sessions_list",
			Title:       "List terminal sessions on a host",
			Operation:   "sessions.list",
			Group:       groupSessions,
			Description: "Read a bounded page of human terminal sessions on one Mesh host without attaching or waking the host.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"all":{"type":"boolean","description":"Include exited and interrupted sessions."},"detail":{"type":"boolean","description":"Include launch command and working directory."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum sessions to return (1 to 500, default 100)."},"page":{"type":"string","description":"Opaque nextPage cursor from this host."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleSessionsList,
		},
		{
			Name:        "mesh_session_inspect",
			Title:       "Passively inspect a terminal session",
			Operation:   "sessions.inspect",
			Group:       groupSessions,
			Description: "Inspect live or saved details of a human terminal session without attaching, stealing, or resizing it. Request preview only when screen lines are needed (capped at 80 by 24).",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","session_id"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"session_id":{"type":"string","description":"Canonical Mesh session ID."},"preview":{"type":"boolean","description":"Include a control-free screen preview capped at 80 by 24."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleSessionInspect,
		},
		{
			Name:        "mesh_services_list",
			Title:       "List Mesh serve routes",
			Operation:   "services.list",
			Group:       groupServices,
			Description: "List live and cached Mesh serve routes across the fleet or on one host without opening service URLs (which could trigger on-demand startup).",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"host":{"type":"string","description":"Optional machine name or exact host ID filter."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleServicesList,
		},
		{
			Name:        "mesh_service_inspect",
			Title:       "Inspect one Mesh serve route",
			Operation:   "services.inspect",
			Group:       groupServices,
			Description: "Inspect one Mesh serve route on a host without opening its URL or triggering on-demand startup.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","name"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"name":{"type":"string","description":"Exact serve route name."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleServiceInspect,
		},
		{
			Name:        "mesh_job_status",
			Title:       "Read durable status of a job",
			Operation:   "sessions.inspect",
			Group:       groupJobs,
			Description: "Read the durable execution receipt and current phase for an exact job handle without touching the process. Treat job handles as opaque. Terminal receipts are retained for at least 30 days; captured output is retained for 7 days.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobStatus,
		},
		{
			Name:        "mesh_job_wait",
			Title:       "Wait for a job to finish",
			Operation:   "sessions.wait",
			Group:       groupJobs,
			Description: "Wait up to 60 seconds for a job to reach a durable terminal result, returning timed_out with the current receipt if still running. Cancelling a wait never cancels or signals the job. Emits notifications/progress when progressToken is set.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"wait_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Maximum wait duration in milliseconds (100 to 60000, default 10000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobWait,
		},
		{
			Name:        "mesh_job_output",
			Title:       "Read bounded job output page",
			Operation:   "sessions.logs",
			Group:       groupJobs,
			Description: "Read one bounded page of job output (default 64 KiB, max 1 MiB) from an opaque cursor. Pipe jobs retain an 8 MiB contiguous suffix per stream (16 MiB for PTY) for 7 days. Expired cursors return logs.cursor_expired with the gap and earliest available cursor; expired output returns result.expired while the receipt remains for 30 days.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"stream":{"type":"string","enum":["stdout","stderr","pty"],"description":"Output stream to read (default stdout)."},"cursor":{"type":"string","description":"Opaque continuation cursor returned by an earlier page."},"max_bytes":{"type":"integer","minimum":1,"maximum":1048576,"description":"Maximum decoded bytes in this page (1 to 1048576, default 65536)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobOutput,
		},
		{
			Name:        "mesh_job_receipt",
			Title:       "Look up a job receipt by request key",
			Operation:   "sessions.request",
			Group:       groupJobs,
			Description: "Look up a durable execution creation receipt by request_key on a host without launching work. Consults the host journal directly, including before catalog publication. Terminal receipts are retained for at least 30 days.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","request_key"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"request_key":{"type":"string","description":"Caller-chosen request key saved before job creation."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobReceipt,
		},
		{
			Name:        "mesh_jobs_list",
			Title:       "List keyed execution jobs on a host",
			Operation:   "jobs.list",
			Group:       groupJobs,
			Description: "List keyed execution jobs on one Mesh host with their opaque mj1 handles and current states without touching any running process.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"all":{"type":"boolean","description":"Include exited and interrupted jobs."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum jobs to return (1 to 500, default 100)."},"page":{"type":"string","description":"Opaque nextPage cursor from this host."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobsList,
		},
		{
			Name:        "mesh_job_input",
			Title:       "Write keyed stdin bytes or EOF to a pipe job",
			Operation:   "sessions.input",
			Group:       groupJobs,
			Description: "Atomically admit up to 1 MiB to a pipe job's open stdin at an expected decimal byte offset, or look up an earlier write_key acknowledgment when offset is omitted. Exact retries return the original receipt; conflicting keys or stale offsets fail without writing.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job","write_key"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"write_key":{"type":"string","description":"Caller-chosen logical write key."},"offset":{"type":"string","description":"Expected nonnegative decimal byte offset; omit to look up write_key without writing."},"data":{"type":"string","description":"UTF-8 text bytes to write (mutually exclusive with data_base64)."},"data_base64":{"type":"string","description":"Base64-encoded bytes to write (mutually exclusive with data)."},"eof":{"type":"boolean","description":"Close pipe stdin after writing these bytes (valid even with empty data)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleJobInput,
		},
		{
			Name:        "mesh_job_signal",
			Title:       "Send a POSIX signal to a job's process group",
			Operation:   "sessions.signal",
			Group:       groupJobs,
			Description: "Deliver a POSIX signal to the process group of an exact job instance handle.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job","signal"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"signal":{"type":"string","enum":["hup","int","kill","quit","term","usr1","usr2"],"description":"Process group signal name."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleJobSignal,
		},
		{
			Name:        "mesh_job_cancel",
			Title:       "Cancel a running job",
			Operation:   "sessions.kill",
			Group:       groupJobs,
			Description: "Durably record kill intent and stop an exact job's process group using hangup-then-insist escalation with the 250 ms descendant drain.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(true),
			Handler:     handleJobCancel,
		},
		{
			Name:        "mesh_job_remove",
			Title:       "Remove finished job output while keeping its receipt",
			Operation:   "sessions.remove",
			Group:       groupJobs,
			Description: "Remove captured output for a finished job while preserving its compact creation receipt and exit details for the 30-day retention floor.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(true),
			Handler:     handleJobRemove,
		},
	}
}
