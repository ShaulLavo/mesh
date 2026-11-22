package mcpnative

import (
	"context"
	"encoding/json"
	"sort"
)

const (
	groupSkill    = "skill"
	groupHosts    = "hosts"
	groupContexts = "contexts"
	groupExec     = "exec"
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

// NewRegistry builds a deterministically sorted native MCP tool registry.
func NewRegistry(specs []ToolSpec) *Registry {
	ordered := append([]ToolSpec(nil), specs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	byName := make(map[string]ToolSpec, len(ordered))
	for _, spec := range ordered {
		byName[spec.Name] = spec
	}
	return &Registry{ordered: ordered, byName: byName}
}

// DefaultRegistry builds the shipped native MCP tool registry sorted by tool name.
func DefaultRegistry() *Registry {
	return NewRegistry(nativeToolSpecs())
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

func mutatingAnnotations() ToolAnnotations {
	return ToolAnnotations{
		ReadOnlyHint:    boolPtr(false),
		DestructiveHint: boolPtr(false),
		IdempotentHint:  boolPtr(false),
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

func openWorldDestructiveAnnotations() ToolAnnotations {
	return ToolAnnotations{
		ReadOnlyHint:    boolPtr(false),
		DestructiveHint: boolPtr(true),
		IdempotentHint:  boolPtr(false),
		OpenWorldHint:   boolPtr(true),
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
			Name:        "mesh_context_open",
			Title:       "Open a durable execution context on a host",
			Operation:   "contexts.open",
			Group:       groupContexts,
			Description: "Open a durable working context (pinned host, cwd, shell, and environment overlay) owned by this application identity. Requires mesh mcp --identity NAME and a caller-chosen request_key; replaying the same key and intent returns the original revision-1 snapshot (call mesh_context_get for current state), while changed intent returns request.key_conflict. Environment values are stored on the host only (max 128 variables, 32 KiB, MESH_* reserved) and never returned. Limits: 64 open contexts per caller and 1,024 per host. A context with no running or uncertain jobs expires 30 days after last use. Treat context handles as opaque.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","request_key","cwd"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"request_key":{"type":"string","description":"Caller-chosen logical request key for idempotent open and receipt recovery."},"cwd":{"type":"string","description":"Absolute clean directory path on the target host."},"shell":{"type":"string","enum":["login","sh","none"],"description":"Shell used for command strings in mesh_exec (default login)."},"env":{"type":"object","additionalProperties":{"type":"string"},"description":"Optional environment overlay stored on the host only (max 128 variables, 32 KiB; MESH_* reserved)."},"label":{"type":"string","description":"Optional display label (max 256 bytes)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: idempotentWriteAnnotations(),
			Handler:     handleContextOpen,
		},
		{
			Name:        "mesh_context_get",
			Title:       "Read one durable execution context",
			Operation:   "contexts.get",
			Group:       groupContexts,
			Description: "Read one durable execution context owned by this application identity using its opaque mc1.<hostId>.<contextId> handle. Returns environment variable names and digest, never values.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["context"],"properties":{"context":{"type":"string","description":"Opaque context handle (mc1.<hostId>.<contextId>)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleContextGet,
		},
		{
			Name:        "mesh_context_list",
			Title:       "List execution contexts on a host",
			Operation:   "contexts.list",
			Group:       groupContexts,
			Description: "List durable execution contexts owned by this application identity on one Mesh host. Returns a byte-bounded page and an optional nextCursor when more contexts remain.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"cursor":{"type":"string","description":"Optional opaque nextCursor returned by a previous mesh_context_list call."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleContextList,
		},
		{
			Name:        "mesh_context_receipt",
			Title:       "Look up a context open receipt by request key",
			Operation:   "contexts.receipt",
			Group:       groupContexts,
			Description: "Look up the original revision-1 context open receipt created with request_key on a host after a lost reply, including after the context has been updated or closed. Call mesh_context_get with the returned handle for current state.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","request_key"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"request_key":{"type":"string","description":"Request key used to open the context."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleContextReceipt,
		},
		{
			Name:        "mesh_context_update",
			Title:       "Update a context at an expected revision",
			Operation:   "contexts.update",
			Group:       groupContexts,
			Description: "Update a context's cwd, environment overlay, or label at expected_revision. Fails with context.conflict if the revision changed. Jobs never mutate a context; only mesh_context_update does.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["context","expected_revision"],"properties":{"context":{"type":"string","description":"Opaque context handle (mc1.<hostId>.<contextId>)."},"expected_revision":{"oneOf":[{"type":"integer","minimum":0},{"type":"string","pattern":"^[0-9]+$"}],"description":"Expected context revision before this update."},"cwd":{"type":"string","description":"Optional new absolute clean working directory on the host."},"set_env":{"type":"object","additionalProperties":{"type":"string"},"description":"Optional environment variables to add or replace."},"unset_env":{"type":"array","items":{"type":"string"},"description":"Optional environment variable names to remove."},"label":{"type":"string","description":"Optional new display label (empty string clears it)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: mutatingAnnotations(),
			Handler:     handleContextUpdate,
		},
		{
			Name:        "mesh_context_close",
			Title:       "Close an execution context",
			Operation:   "contexts.close",
			Group:       groupContexts,
			Description: "Close a durable execution context so no new jobs can start in it. Running jobs keep running and remain addressable by their job handles until their receipts expire.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["context"],"properties":{"context":{"type":"string","description":"Opaque context handle (mc1.<hostId>.<contextId>)."},"expected_revision":{"oneOf":[{"type":"integer","minimum":0},{"type":"string","pattern":"^[0-9]+$"}],"description":"Optional expected context revision; fails with context.conflict if stale."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(true),
			Handler:     handleContextClose,
		},
		{
			Name:        "mesh_exec",
			Title:       "Run a keyed pipe command in an execution context",
			Operation:   "sessions.create",
			Group:       groupExec,
			Description: "Run a keyed pipe job inside a durable execution context (requires mesh mcp --identity NAME). Pass exactly one of command (shell string, refused for shell=none) or argv (direct executable array). Waits up to wait_ms (default 10000, max 60000) and returns exit details plus up to output_bytes (default 16384, max 262144) of stdout and stderr with continuation cursors, or the opaque mj1 job handle with state=running if still in progress. Emits notifications/progress with observed byte counts when progressToken is set. Child cd/export never modifies the context. Pipe jobs have no controlling terminal. Terminal receipts are retained for 30 days; captured output (8 MiB suffix per stream) is retained for 7 days.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["context","request_key"],"oneOf":[{"required":["command"],"not":{"required":["argv"]}},{"required":["argv"],"not":{"required":["command"]}}],"properties":{"context":{"type":"string","description":"Opaque context handle (mc1.<hostId>.<contextId>)."},"request_key":{"type":"string","description":"Caller-chosen logical request key for idempotent dispatch and recovery via mesh_job_receipt."},"command":{"type":"string","minLength":1,"description":"Shell command string (mutually exclusive with argv; refused when context shell is none)."},"argv":{"type":"array","items":{"type":"string"},"minItems":1,"description":"Direct executable and arguments (mutually exclusive with command)."},"cwd":{"type":"string","description":"Optional absolute or context-relative working directory for this job only."},"env":{"type":"object","additionalProperties":{"type":"string"},"description":"Optional per-job environment overlay."},"stdin":{"type":"string","enum":["closed","open"],"description":"Pipe stdin mode: closed (default, /dev/null) or open (write with mesh_job_input)."},"wait_ms":{"type":"integer","minimum":0,"maximum":60000,"description":"Maximum milliseconds to wait for completion before returning running state (0 to 60000, default 10000)."},"output_bytes":{"type":"integer","minimum":1,"maximum":262144,"description":"Maximum initial stdout and stderr bytes each to include in the result (1 to 262144, default 16384)."}}}`),
			Annotations: openWorldDestructiveAnnotations(),
			Handler:     handleExec,
		},
		{
			Name:        "mesh_job_status",
			Title:       "Read durable status of a job",
			Operation:   "sessions.inspect",
			Group:       groupJobs,
			Description: "Read the durable execution receipt and current phase for an exact job handle without touching the process. Treat job handles as opaque. Terminal receipts are retained for at least 30 days; captured output is retained for 7 days.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobStatus,
		},
		{
			Name:        "mesh_job_wait",
			Title:       "Wait for a job to finish",
			Operation:   "sessions.wait",
			Group:       groupJobs,
			Description: "Wait up to 60 seconds for a job to reach a durable terminal result, returning timed_out with the current receipt if still running. Cancelling a wait never cancels or signals the job. Emits notifications/progress when progressToken is set.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"wait_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Maximum wait duration in milliseconds (100 to 60000, default 10000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobWait,
		},
		{
			Name:        "mesh_job_output",
			Title:       "Read bounded job output page",
			Operation:   "sessions.logs",
			Group:       groupJobs,
			Description: "Read one bounded page of job output (default 64 KiB, max 1 MiB) from an opaque cursor. Pipe jobs retain an 8 MiB contiguous suffix per stream (16 MiB for PTY) for 7 days. Expired cursors return logs.cursor_expired with the gap and earliest available cursor; expired output returns result.expired while the receipt remains for 30 days.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"stream":{"type":"string","enum":["stdout","stderr","pty"],"description":"Output stream to read (default stdout)."},"cursor":{"type":"string","description":"Opaque continuation cursor returned by an earlier page."},"max_bytes":{"type":"integer","minimum":1,"maximum":1048576,"description":"Maximum decoded bytes in this page (1 to 1048576, default 65536)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobOutput,
		},
		{
			Name:        "mesh_job_receipt",
			Title:       "Look up a job receipt by request key",
			Operation:   "sessions.request",
			Group:       groupJobs,
			Description: "Look up a durable execution creation receipt by request_key without launching work. Pass context (mc1 handle) for context jobs created with mesh_exec, or host for account-scoped jobs. Consults the host journal directly, including before catalog publication and after context closure. Terminal receipts are retained for at least 30 days.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["request_key"],"properties":{"context":{"type":"string","description":"Opaque context handle (mc1.<hostId>.<contextId>) for context-scoped job receipts."},"host":{"type":"string","description":"Machine name or exact host ID (required when context is omitted)."},"request_key":{"type":"string","description":"Caller-chosen request key saved before job creation."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobReceipt,
		},
		{
			Name:        "mesh_jobs_list",
			Title:       "List keyed execution jobs on a host",
			Operation:   "jobs.list",
			Group:       groupJobs,
			Description: "List keyed execution jobs on one Mesh host (or scoped to one context handle) with their opaque mj1 handles and current states without touching any running process.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"host":{"type":"string","description":"Machine name or exact host ID (implied when context is set)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to filter jobs to one context."},"all":{"type":"boolean","description":"Include exited and interrupted jobs."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum jobs to return (1 to 500, default 100)."},"page":{"type":"string","description":"Opaque nextPage cursor from this host."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleJobsList,
		},
		{
			Name:        "mesh_job_input",
			Title:       "Write keyed stdin bytes or EOF to a pipe job",
			Operation:   "sessions.input",
			Group:       groupJobs,
			Description: "Atomically admit up to 1 MiB to a pipe job's open stdin at an expected decimal byte offset, or look up an earlier write_key acknowledgment when offset is omitted. Exact retries return the original receipt; conflicting keys or stale offsets fail without writing.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job","write_key"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"write_key":{"type":"string","description":"Caller-chosen logical write key."},"offset":{"type":"string","description":"Expected nonnegative decimal byte offset; omit to look up write_key without writing."},"data":{"type":"string","description":"UTF-8 text bytes to write (mutually exclusive with data_base64)."},"data_base64":{"type":"string","description":"Base64-encoded bytes to write (mutually exclusive with data)."},"eof":{"type":"boolean","description":"Close pipe stdin after writing these bytes (valid even with empty data)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleJobInput,
		},
		{
			Name:        "mesh_job_signal",
			Title:       "Send a POSIX signal to a job's process group",
			Operation:   "sessions.signal",
			Group:       groupJobs,
			Description: "Deliver a POSIX signal to the process group of an exact job instance handle.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job","signal"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"signal":{"type":"string","enum":["hup","int","kill","quit","term","usr1","usr2"],"description":"Process group signal name."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleJobSignal,
		},
		{
			Name:        "mesh_job_cancel",
			Title:       "Cancel a running job",
			Operation:   "sessions.kill",
			Group:       groupJobs,
			Description: "Durably record kill intent and stop an exact job's process group using hangup-then-insist escalation with the 250 ms descendant drain.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(true),
			Handler:     handleJobCancel,
		},
		{
			Name:        "mesh_job_remove",
			Title:       "Remove finished job output while keeping its receipt",
			Operation:   "sessions.remove",
			Group:       groupJobs,
			Description: "Remove captured output for a finished job while preserving its compact creation receipt and exit details for the 30-day retention floor.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(true),
			Handler:     handleJobRemove,
		},
	}
}
