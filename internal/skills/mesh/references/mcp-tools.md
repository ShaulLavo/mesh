# Native MCP tools

Generated from the native MCP tool registry (`internal/mcpnative`). Regenerate with `go generate ./internal/mcpnative`.

| Tool | Linked operation | Mode | Summary |
| --- | --- | --- | --- |
| `mesh_copy` | `files.copy` | destructive, idempotent | Copy a file or directory to or from a host |
| `mesh_file_list` | `files.list` | read-only, idempotent | List a directory |
| `mesh_file_patch` | `files.patch` | destructive | Apply exact text edits to a file |
| `mesh_file_read` | `files.read` | read-only, idempotent | Read a page of a regular file |
| `mesh_file_search` | `files.search` | read-only, idempotent | Search text files with a regular expression |
| `mesh_file_write` | `files.write` | destructive | Write or create a file atomically |
| `mesh_fleet_status` | `profile.status` | read-only, idempotent | Read signed fleet profile delivery status |
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
| `mesh_profile_get` | `profile.show` | read-only, idempotent | Read this machine's host or fleet notes |
| `mesh_profile_set` | `profile.set` | destructive | Replace this machine's host or fleet notes |
| `mesh_service_inspect` | `services.inspect` | read-only, idempotent | Inspect one Mesh serve route |
| `mesh_services_list` | `services.list` | read-only, idempotent | List Mesh serve routes |
| `mesh_session_inspect` | `sessions.inspect` | read-only, idempotent | Passively inspect a terminal session |
| `mesh_sessions_list` | `sessions.list` | read-only, idempotent | List terminal sessions on a host |
| `mesh_skill` | `skill.show` | read-only, idempotent | Read embedded Mesh skill instructions |

## mesh_copy

Copy a file or directory between this machine and one remote Mesh host over an authenticated connection, with no size limit beyond disk space. upload sends local_path to remote_path; download fetches remote_path to local_path. Files are replaced atomically, directories merge, permission bits are kept, and symlinks and special files are refused. Use mesh_file_write for small files.

- Linked operation: `files.copy`
- Hints: destructive, idempotent
- Arguments:
  - `direction` (`string`, required) (upload, download): upload: local to host. download: host to local.
  - `host` (`string`, required): Machine name or exact host ID of the remote host.
  - `local_path` (`string`, required): Absolute path on the machine running mesh mcp.
  - `recursive` (`boolean`, optional): Copy a directory and its contents.
  - `remote_path` (`string`, required): Absolute path on the host.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 600000, default 120000).

## mesh_file_list

List one directory on a host in name order, without following symlinks. Entries report file, dir, symlink or other. Pages hold up to 1000 entries; nextCursor continues after the last name returned.

- Linked operation: `files.list`
- Hints: read-only, idempotent
- Arguments:
  - `context` (`string`, optional): Opaque context handle (mc1.<hostId>.<contextId>) whose working directory resolves a relative path. Needs an application identity; the owning host authorizes it.
  - `cursor` (`string`, optional): Opaque nextCursor from the previous page of this directory.
  - `host` (`string`, optional): Machine name or exact host ID. Optional when context names the host.
  - `limit` (`integer`, optional): Entries per page (1 to 1000, default 200).
  - `path` (`string`, required): Absolute path, or a path relative to the context's working directory (at most 4096 bytes).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_file_patch

Apply up to 64 exact old_text to new_text edits, in order, to a file of at most 4 MiB whose SHA-256 matches expected_sha256. Each old_text must occur exactly once in the text as earlier edits left it (overlapping occurrences count as several); otherwise file.patch_mismatch answers and nothing is written.

- Linked operation: `files.patch`
- Hints: destructive
- Arguments:
  - `context` (`string`, optional): Opaque context handle (mc1.<hostId>.<contextId>) whose working directory resolves a relative path. Needs an application identity; the owning host authorizes it.
  - `edits` (`array`, required): 
  - `expected_sha256` (`string`, required): Lowercase SHA-256 of the file's current bytes, from mesh_file_read or an earlier write result.
  - `host` (`string`, optional): Machine name or exact host ID. Optional when context names the host.
  - `path` (`string`, required): Absolute path, or a path relative to the context's working directory (at most 4096 bytes).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_file_read

Read one page (at most 1 MiB) of a regular file on a host as the daemon account. The page is utf-8 text when exact, otherwise base64, and a text page never splits a character. Offsets are decimal strings. nextCursor continues the read and is bound to the host, account, path and file revision, so a replaced or changed file answers file.changed. sha256 (files up to 4 MiB, or any size with digest) is what mesh_file_write and mesh_file_patch need. Symlinks, devices, sockets and pipes are refused.

- Linked operation: `files.read`
- Hints: read-only, idempotent
- Arguments:
  - `context` (`string`, optional): Opaque context handle (mc1.<hostId>.<contextId>) whose working directory resolves a relative path. Needs an application identity; the owning host authorizes it.
  - `cursor` (`string`, optional): Opaque nextCursor from the previous page of this file.
  - `digest` (`boolean`, optional): Also hash a file larger than 4 MiB.
  - `host` (`string`, optional): Machine name or exact host ID. Optional when context names the host.
  - `max_bytes` (`integer`, optional): Page size in bytes (1 to 1048576, default 1048576).
  - `offset` (`string`, optional): First byte to read, as a nonnegative decimal string (default 0). Not with cursor.
  - `path` (`string`, required): Absolute path, or a path relative to the context's working directory (at most 4096 bytes).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_file_search

Find lines matching a Go regular expression in the text files under a directory (or in one file) on a host. Never runs a shell or follows symlinks. Bounded: at most 10,000 directory entries visited, 1,000 matches and 1 MiB of encoded matches returned, 256 MiB read, files over 4 MiB and binary files skipped. truncated names the bound that stopped the search.

- Linked operation: `files.search`
- Hints: read-only, idempotent
- Arguments:
  - `context` (`string`, optional): Opaque context handle (mc1.<hostId>.<contextId>) whose working directory resolves a relative path. Needs an application identity; the owning host authorizes it.
  - `glob` (`string`, optional): Only search files whose base name matches this glob.
  - `host` (`string`, optional): Machine name or exact host ID. Optional when context names the host.
  - `max_matches` (`integer`, optional): Stop after this many matches (1 to 1000, default 1000).
  - `path` (`string`, required): Absolute path, or a path relative to the context's working directory (at most 4096 bytes).
  - `pattern` (`string`, required): Go regular expression (RE2 syntax) matched against each line.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_file_write

Replace or create a regular file on a host with at most 1 MiB, through a temporary file and an atomic install in the same directory. Give exactly one of expected_sha256 (replace a file whose digest matches) or create_only (fail if the name exists). A failed precondition answers file.conflict and changes nothing. The result's revision and sha256 describe the version this call installed. Use mesh_copy for larger transfers.

- Linked operation: `files.write`
- Hints: destructive
- Arguments:
  - `content` (`string`, optional): New contents as UTF-8 text. Not with content_base64.
  - `content_base64` (`string`, optional): New contents as standard base64 for binary data. Not with content.
  - `context` (`string`, optional): Opaque context handle (mc1.<hostId>.<contextId>) whose working directory resolves a relative path. Needs an application identity; the owning host authorizes it.
  - `create_only` (`boolean`, optional): Create the file and fail with file.conflict if it exists.
  - `expected_sha256` (`string`, optional): Lowercase SHA-256 of the file's current bytes, from mesh_file_read or an earlier write result.
  - `host` (`string`, optional): Machine name or exact host ID. Optional when context names the host.
  - `path` (`string`, required): Absolute path, or a path relative to the context's working directory (at most 4096 bytes).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_fleet_status

Read this machine's signed fleet profile replica and the delivery state to each saved fleet member, from local files only. Never contacts members.

- Linked operation: `profile.status`
- Hints: read-only, idempotent
- Arguments: none

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

## mesh_profile_get

Read the owner-written notes of this machine (scope host) or the signed fleet notes replica (scope fleet) without contacting peers. The revision is what mesh_profile_set needs as expected_revision; it is null for an absent profile.

- Linked operation: `profile.show`
- Hints: read-only, idempotent
- Arguments:
  - `scope` (`string`, optional) (host, fleet): host (default) or fleet.

## mesh_profile_set

Replace the notes of this machine (scope host) or publish the signed fleet notes (scope fleet, from a fleet administrator machine) at an expected revision. expected_revision is required (0 for an absent profile); a stale revision answers profile.conflict and changes nothing. The profile document is replaced whole, not merged. Fleet notes are queued to saved members and then delivered; mesh_fleet_status shows delivery.

- Linked operation: `profile.set`
- Hints: destructive
- Arguments:
  - `expected_revision` (`integer`, required): Revision read from mesh_profile_get; 0 for an absent profile.
  - `profile` (`object`, required): 
  - `scope` (`string`, optional) (host, fleet): host (default) or fleet.

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
