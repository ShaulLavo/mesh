# Native MCP tools

Generated from the native MCP tool registry (`internal/mcpnative`). Regenerate with `go generate ./internal/mcpnative`.

| Tool | Linked operation | Mode | Summary |
| --- | --- | --- | --- |
| `mesh_context_close` | `contexts.close` | destructive, idempotent | Close an execution context |
| `mesh_context_get` | `contexts.get` | read-only, idempotent | Read one durable execution context |
| `mesh_context_list` | `contexts.list` | read-only, idempotent | List execution contexts on a host |
| `mesh_context_open` | `contexts.open` | idempotent | Open a durable execution context on a host |
| `mesh_context_receipt` | `contexts.receipt` | read-only, idempotent | Look up a context open receipt by request key |
| `mesh_context_update` | `contexts.update` | mutating | Update a context at an expected revision |
| `mesh_exec` | `sessions.create` | destructive, open-world | Run a keyed pipe command in an execution context |
| `mesh_host_inspect` | `hosts.inspect` | read-only, idempotent | Inspect one Mesh host and its notes |
| `mesh_host_wake` | `hosts.wake` | idempotent | Wake a Mesh host |
| `mesh_hosts_list` | `hosts.list` | read-only, idempotent | List Mesh hosts, reachability, and capabilities |
| `mesh_job_cancel` | `sessions.kill` | destructive, idempotent | Cancel a running job |
| `mesh_job_input` | `sessions.input` | destructive | Write keyed stdin bytes or EOF to a pipe job |
| `mesh_job_output` | `sessions.logs` | read-only, idempotent | Read bounded job output page |
| `mesh_job_receipt` | `sessions.request` | read-only, idempotent | Look up a job receipt by request key |
| `mesh_job_remove` | `sessions.remove` | destructive, idempotent | Remove finished job output while keeping its receipt |
| `mesh_job_signal` | `sessions.signal` | destructive | Send a POSIX signal to a job's process group |
| `mesh_job_status` | `sessions.inspect` | read-only, idempotent | Read durable status of a job |
| `mesh_job_wait` | `sessions.wait` | read-only, idempotent | Wait for a job to finish |
| `mesh_jobs_list` | `jobs.list` | read-only, idempotent | List keyed execution jobs on a host |
| `mesh_service_inspect` | `services.inspect` | read-only, idempotent | Inspect one Mesh serve route |
| `mesh_services_list` | `services.list` | read-only, idempotent | List Mesh serve routes |
| `mesh_session_inspect` | `sessions.inspect` | read-only, idempotent | Passively inspect a terminal session |
| `mesh_sessions_list` | `sessions.list` | read-only, idempotent | List terminal sessions on a host |
| `mesh_skill` | `skill.show` | read-only, idempotent | Read embedded Mesh skill instructions |

## mesh_context_close

Close a durable execution context so no new jobs can start in it. Running jobs keep running and remain addressable by their job handles until their receipts expire.

- Linked operation: `contexts.close`
- Hints: destructive, idempotent
- Arguments:
  - `context` (`string`, required): Opaque context handle (mc1.<hostId>.<contextId>).
  - `expected_revision` (`integer|string`, optional): Optional expected context revision; fails with context.conflict if stale.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_context_get

Read one durable execution context owned by this application identity using its opaque mc1.<hostId>.<contextId> handle. Returns environment variable names and digest, never values.

- Linked operation: `contexts.get`
- Hints: read-only, idempotent
- Arguments:
  - `context` (`string`, required): Opaque context handle (mc1.<hostId>.<contextId>).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_context_list

List durable execution contexts owned by this application identity on one Mesh host. Returns a byte-bounded page and an optional nextCursor when more contexts remain.

- Linked operation: `contexts.list`
- Hints: read-only, idempotent
- Arguments:
  - `cursor` (`string`, optional): Optional opaque nextCursor returned by a previous mesh_context_list call.
  - `host` (`string`, required): Machine name or exact host ID.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_context_open

Open a durable working context (pinned host, cwd, shell, and environment overlay) owned by this application identity. Requires mesh mcp --identity NAME and a caller-chosen request_key; replaying the same key and intent returns the original revision-1 snapshot (call mesh_context_get for current state), while changed intent returns request.key_conflict. Environment values are stored on the host only (max 128 variables, 32 KiB, MESH_* reserved) and never returned. Limits: 64 open contexts per caller and 1,024 per host. A context with no running or uncertain jobs expires 30 days after last use. Treat context handles as opaque.

- Linked operation: `contexts.open`
- Hints: idempotent
- Arguments:
  - `cwd` (`string`, required): Absolute clean directory path on the target host.
  - `env` (`object`, optional): Optional environment overlay stored on the host only (max 128 variables, 32 KiB; MESH_* reserved).
  - `host` (`string`, required): Machine name or exact host ID.
  - `label` (`string`, optional): Optional display label (max 256 bytes).
  - `request_key` (`string`, required): Caller-chosen logical request key for idempotent open and receipt recovery.
  - `shell` (`string`, optional) (login, sh, none): Shell used for command strings in mesh_exec (default login).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_context_receipt

Look up the original revision-1 context open receipt created with request_key on a host after a lost reply, including after the context has been updated or closed. Call mesh_context_get with the returned handle for current state.

- Linked operation: `contexts.receipt`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID.
  - `request_key` (`string`, required): Request key used to open the context.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_context_update

Update a context's cwd, environment overlay, or label at expected_revision. Fails with context.conflict if the revision changed. Jobs never mutate a context; only mesh_context_update does.

- Linked operation: `contexts.update`
- Hints: mutating
- Arguments:
  - `context` (`string`, required): Opaque context handle (mc1.<hostId>.<contextId>).
  - `cwd` (`string`, optional): Optional new absolute clean working directory on the host.
  - `expected_revision` (`integer|string`, required): Expected context revision before this update.
  - `label` (`string`, optional): Optional new display label (empty string clears it).
  - `set_env` (`object`, optional): Optional environment variables to add or replace.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).
  - `unset_env` (`array`, optional): Optional environment variable names to remove.

## mesh_exec

Run a keyed pipe job inside a durable execution context (requires mesh mcp --identity NAME). Pass exactly one of command (shell string, refused for shell=none) or argv (direct executable array). Waits up to wait_ms (default 10000, max 60000) and returns exit details plus up to output_bytes (default 16384, max 262144) of stdout and stderr with continuation cursors, or the opaque mj1 job handle with state=running if still in progress. Emits notifications/progress with observed byte counts when progressToken is set. Child cd/export never modifies the context. Pipe jobs have no controlling terminal. Terminal receipts are retained for 30 days; captured output (8 MiB suffix per stream) is retained for 7 days.

- Linked operation: `sessions.create`
- Hints: destructive, open-world
- Arguments:
  - `argv` (`array`, optional): Direct executable and arguments (mutually exclusive with command).
  - `command` (`string`, optional): Shell command string (mutually exclusive with argv; refused when context shell is none).
  - `context` (`string`, required): Opaque context handle (mc1.<hostId>.<contextId>).
  - `cwd` (`string`, optional): Optional absolute or context-relative working directory for this job only.
  - `env` (`object`, optional): Optional per-job environment overlay.
  - `output_bytes` (`integer`, optional): Maximum initial stdout and stderr bytes each to include in the result (1 to 262144, default 16384).
  - `request_key` (`string`, required): Caller-chosen logical request key for idempotent dispatch and recovery via mesh_job_receipt.
  - `stdin` (`string`, optional) (closed, open): Pipe stdin mode: closed (default, /dev/null) or open (write with mesh_job_input).
  - `wait_ms` (`integer`, optional): Maximum milliseconds to wait for completion before returning running state (0 to 60000, default 10000).

## mesh_host_inspect

Inspect one Mesh host for full owner notes, fleet notes, capabilities, and current access level. Never wakes the host or starts a daemon.

- Linked operation: `hosts.inspect`
- Hints: read-only, idempotent
- Arguments:
  - `cached` (`boolean`, optional): Read cached observations without dialing.
  - `host` (`string`, required): Machine name or exact host ID.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_host_wake

Send a wake-on-LAN request for an approved Mesh host through an online peer. Idempotent when the host is already online.

- Linked operation: `hosts.wake`
- Hints: idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_hosts_list

List configured Mesh hosts with reachability, freshness, protocol capability booleans, and summary host and fleet notes. Never wakes a host or starts a daemon.

- Linked operation: `hosts.list`
- Hints: read-only, idempotent
- Arguments:
  - `cached` (`boolean`, optional): Read local files and cached observations without dialing hosts.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 30000, default 2000).

## mesh_job_cancel

Durably record kill intent and stop an exact job's process group using hangup-then-insist escalation with the 250 ms descendant drain.

- Linked operation: `sessions.kill`
- Hints: destructive, idempotent
- Arguments:
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_input

Atomically admit up to 1 MiB to a pipe job's open stdin at an expected decimal byte offset, or look up an earlier write_key acknowledgment when offset is omitted. Exact retries return the original receipt; conflicting keys or stale offsets fail without writing.

- Linked operation: `sessions.input`
- Hints: destructive
- Arguments:
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `data` (`string`, optional): UTF-8 text bytes to write (mutually exclusive with data_base64).
  - `data_base64` (`string`, optional): Base64-encoded bytes to write (mutually exclusive with data).
  - `eof` (`boolean`, optional): Close pipe stdin after writing these bytes (valid even with empty data).
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `offset` (`string`, optional): Expected nonnegative decimal byte offset; omit to look up write_key without writing.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).
  - `write_key` (`string`, required): Caller-chosen logical write key.

## mesh_job_output

Read one bounded page of job output (default 64 KiB, max 1 MiB) from an opaque cursor. Pipe jobs retain an 8 MiB contiguous suffix per stream (16 MiB for PTY) for 7 days. Expired cursors return logs.cursor_expired with the gap and earliest available cursor; expired output returns result.expired while the receipt remains for 30 days.

- Linked operation: `sessions.logs`
- Hints: read-only, idempotent
- Arguments:
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `cursor` (`string`, optional): Opaque continuation cursor returned by an earlier page.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `max_bytes` (`integer`, optional): Maximum decoded bytes in this page (1 to 1048576, default 65536).
  - `stream` (`string`, optional) (stdout, stderr, pty): Output stream to read (default stdout).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_receipt

Look up a durable execution creation receipt by request_key without launching work. Pass context (mc1 handle) for context jobs created with mesh_exec, or host for account-scoped jobs. Consults the host journal directly, including before catalog publication and after context closure. Terminal receipts are retained for at least 30 days.

- Linked operation: `sessions.request`
- Hints: read-only, idempotent
- Arguments:
  - `context` (`string`, optional): Opaque context handle (mc1.<hostId>.<contextId>) for context-scoped job receipts.
  - `host` (`string`, optional): Machine name or exact host ID (required when context is omitted).
  - `request_key` (`string`, required): Caller-chosen request key saved before job creation.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_remove

Remove captured output for a finished job while preserving its compact creation receipt and exit details for the 30-day retention floor.

- Linked operation: `sessions.remove`
- Hints: destructive, idempotent
- Arguments:
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_signal

Deliver a POSIX signal to the process group of an exact job instance handle.

- Linked operation: `sessions.signal`
- Hints: destructive
- Arguments:
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `signal` (`string`, required) (hup, int, kill, quit, term, usr1, usr2): Process group signal name.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_status

Read the durable execution receipt and current phase for an exact job handle without touching the process. Treat job handles as opaque. Terminal receipts are retained for at least 30 days; captured output is retained for 7 days.

- Linked operation: `sessions.inspect`
- Hints: read-only, idempotent
- Arguments:
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_wait

Wait up to 60 seconds for a job to reach a durable terminal result, returning timed_out with the current receipt if still running. Cancelling a wait never cancels or signals the job. Emits notifications/progress when progressToken is set.

- Linked operation: `sessions.wait`
- Hints: read-only, idempotent
- Arguments:
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `wait_ms` (`integer`, optional): Maximum wait duration in milliseconds (100 to 60000, default 10000).

## mesh_jobs_list

List keyed execution jobs on one Mesh host (or scoped to one context handle) with their opaque mj1 handles and current states without touching any running process.

- Linked operation: `jobs.list`
- Hints: read-only, idempotent
- Arguments:
  - `all` (`boolean`, optional): Include exited and interrupted jobs.
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to filter jobs to one context.
  - `host` (`string`, optional): Machine name or exact host ID (implied when context is set).
  - `limit` (`integer`, optional): Maximum jobs to return (1 to 500, default 100).
  - `page` (`string`, optional): Opaque nextPage cursor from this host.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_service_inspect

Inspect one Mesh serve route on a host without opening its URL or triggering on-demand startup.

- Linked operation: `services.inspect`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID.
  - `name` (`string`, required): Exact serve route name.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_services_list

List live and cached Mesh serve routes across the fleet or on one host without opening service URLs (which could trigger on-demand startup).

- Linked operation: `services.list`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, optional): Optional machine name or exact host ID filter.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_session_inspect

Inspect live or saved details of a human terminal session without attaching, stealing, or resizing it. Request preview only when screen lines are needed (capped at 80 by 24).

- Linked operation: `sessions.inspect`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID.
  - `preview` (`boolean`, optional): Include a control-free screen preview capped at 80 by 24.
  - `session_id` (`string`, required): Canonical Mesh session ID.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_sessions_list

Read a bounded page of human terminal sessions on one Mesh host without attaching or waking the host.

- Linked operation: `sessions.list`
- Hints: read-only, idempotent
- Arguments:
  - `all` (`boolean`, optional): Include exited and interrupted sessions.
  - `detail` (`boolean`, optional): Include launch command and working directory.
  - `host` (`string`, required): Machine name or exact host ID.
  - `limit` (`integer`, optional): Maximum sessions to return (1 to 500, default 100).
  - `page` (`string`, optional): Opaque nextPage cursor from this host.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_skill

Read Mesh's embedded agent skill instructions and reference documents without reading configuration or contacting any daemon.

- Linked operation: `skill.show`
- Hints: read-only, idempotent
- Arguments:
  - `file` (`string`, optional): Embedded skill file path such as SKILL.md, references/commands.md, references/safety.md, or references/mcp-tools.md. Defaults to SKILL.md.
