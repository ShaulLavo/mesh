# Native MCP tools

Generated from the native MCP tool registry (`internal/mcpnative`). Regenerate with `go generate ./internal/mcpnative`.

| Tool | Linked operation | Mode | Summary |
| --- | --- | --- | --- |
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
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_input

Atomically admit up to 1 MiB to a pipe job's open stdin at an expected decimal byte offset, or look up an earlier write_key acknowledgment when offset is omitted. Exact retries return the original receipt; conflicting keys or stale offsets fail without writing.

- Linked operation: `sessions.input`
- Hints: destructive
- Arguments:
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
  - `cursor` (`string`, optional): Opaque continuation cursor returned by an earlier page.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `max_bytes` (`integer`, optional): Maximum decoded bytes in this page (1 to 1048576, default 65536).
  - `stream` (`string`, optional) (stdout, stderr, pty): Output stream to read (default stdout).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_receipt

Look up a durable execution creation receipt by request_key on a host without launching work. Consults the host journal directly, including before catalog publication. Terminal receipts are retained for at least 30 days.

- Linked operation: `sessions.request`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID.
  - `request_key` (`string`, required): Caller-chosen request key saved before job creation.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_remove

Remove captured output for a finished job while preserving its compact creation receipt and exit details for the 30-day retention floor.

- Linked operation: `sessions.remove`
- Hints: destructive, idempotent
- Arguments:
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_signal

Deliver a POSIX signal to the process group of an exact job instance handle.

- Linked operation: `sessions.signal`
- Hints: destructive
- Arguments:
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `signal` (`string`, required) (hup, int, kill, quit, term, usr1, usr2): Process group signal name.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_status

Read the durable execution receipt and current phase for an exact job handle without touching the process. Treat job handles as opaque. Terminal receipts are retained for at least 30 days; captured output is retained for 7 days.

- Linked operation: `sessions.inspect`
- Hints: read-only, idempotent
- Arguments:
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_job_wait

Wait up to 60 seconds for a job to reach a durable terminal result, returning timed_out with the current receipt if still running. Cancelling a wait never cancels or signals the job. Emits notifications/progress when progressToken is set.

- Linked operation: `sessions.wait`
- Hints: read-only, idempotent
- Arguments:
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `wait_ms` (`integer`, optional): Maximum wait duration in milliseconds (100 to 60000, default 10000).

## mesh_jobs_list

List keyed execution jobs on one Mesh host with their opaque mj1 handles and current states without touching any running process.

- Linked operation: `jobs.list`
- Hints: read-only, idempotent
- Arguments:
  - `all` (`boolean`, optional): Include exited and interrupted jobs.
  - `host` (`string`, required): Machine name or exact host ID.
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
