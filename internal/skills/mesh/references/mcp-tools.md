# Native MCP tools

Generated from the native MCP tool registry (`internal/mcpnative`). Regenerate with `go generate ./internal/mcpnative`.

| Tool | Linked operation | Mode | Summary |
| --- | --- | --- | --- |
| `mesh_apps_list` | `apps.list` | read-only, idempotent | List a host's temporary apps |
| `mesh_context_close` | `contexts.close` | destructive, idempotent | Close an execution context |
| `mesh_context_get` | `contexts.get` | read-only, idempotent | Read one durable execution context |
| `mesh_context_list` | `contexts.list` | read-only, idempotent | List execution contexts on a host |
| `mesh_context_open` | `contexts.open` | idempotent | Open a durable execution context on a host |
| `mesh_context_receipt` | `contexts.receipt` | read-only, idempotent | Look up a context open receipt by request key |
| `mesh_context_update` | `contexts.update` | mutating | Update a context at an expected revision |
| `mesh_copy` | `files.copy` | destructive | Copy a file or directory to or from a host |
| `mesh_exec` | `sessions.create` | destructive, open-world | Run a keyed pipe command in an execution context |
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
| `mesh_service_publish` | `services.publish` | destructive, open-world | Publish or replace a private route |
| `mesh_service_start` | `services.start` | destructive, idempotent | Start an on-demand route |
| `mesh_service_stop` | `services.stop` | destructive, idempotent | Stop an on-demand route |
| `mesh_service_unpublish` | `services.unpublish` | destructive, idempotent | Remove a private route |
| `mesh_services_list` | `services.list` | read-only, idempotent | List Mesh serve routes |
| `mesh_session_hibernate` | `sessions.hibernate` | destructive, idempotent | Hibernate a detached agent session |
| `mesh_session_inspect` | `sessions.inspect` | read-only, idempotent | Passively inspect a terminal session |
| `mesh_session_recover` | `sessions.recover` | destructive | Start a detached replacement for an interrupted session |
| `mesh_sessions_list` | `sessions.list` | read-only, idempotent | List terminal sessions on a host |
| `mesh_skill` | `skill.show` | read-only, idempotent | Read embedded Mesh skill instructions |
| `mesh_terminal_input` | `sessions.input` | destructive | Type keyed input into a terminal job |
| `mesh_terminal_open` | `sessions.create` | destructive, open-world | Open a keyed terminal job in an execution context |
| `mesh_terminal_resize` | `sessions.resize` | destructive, idempotent | Resize a terminal job |
| `mesh_terminal_screen` | `sessions.screen` | read-only, idempotent | Read a terminal job's rendered screen |

## mesh_apps_list

List the temporary apps a host owns (at most 200), with state, expiry and URL. App owner operations run through the owning host's local socket, reached over Mesh SSH for another machine, the same as mesh app list.

- Linked operation: `apps.list`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, required): Machine name, exact host ID, or local for this machine.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

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

## mesh_copy

Copy a file or directory between this machine and one remote Mesh host over an authenticated connection, with no size limit beyond disk space. upload sends local_path to remote_path; download fetches remote_path to local_path. Files are replaced atomically, directories merge, permission bits are kept, and symlinks and special files are refused. Repeating a directory copy into an existing directory nests the source inside it, so it is not safe to repeat. Use mesh_file_write for small files.

- Linked operation: `files.copy`
- Hints: destructive
- Arguments:
  - `direction` (`string`, required) (upload, download): upload: local to host. download: host to local.
  - `host` (`string`, required): Machine name or exact host ID of the remote host.
  - `local_path` (`string`, required): Absolute path on the machine running mesh mcp.
  - `recursive` (`boolean`, optional): Copy a directory and its contents.
  - `remote_path` (`string`, required): Absolute path on the host.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 600000, default 120000).

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
  - `edits` (`array`, required): Edits applied in order; each old_text must occur exactly once.
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
  - `profile` (`object`, required): The whole notes document; it replaces the current one.
  - `scope` (`string`, optional) (host, fleet): host (default) or fleet.

## mesh_service_inspect

Inspect one Mesh serve route on a host without opening its URL or triggering on-demand startup.

- Linked operation: `services.inspect`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID.
  - `name` (`string`, required): Exact serve route name.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_service_publish

Publish one private, tailnet-only route on a host, the same as mesh serve HOST TARGET. target is a port on that host, or a directory on it to serve as files. With run, the host starts the command on the first connection through Mesh and stops it when idle (idle_ms, default 15 minutes), or keeps it running with keep_running. run on another machine needs an absolute cwd on that machine. Replacing a route keeps its private hostname and recipe lifetime unless given. The host previews the definition first and publishes exactly what it previewed.

- Linked operation: `services.publish`
- Hints: destructive, open-world
- Arguments:
  - `cwd` (`string`, optional): Directory run starts in, absolute on the host.
  - `env` (`array`, optional): KEY=VALUE entries added to the environment of run.
  - `files` (`boolean`, optional): Enable directory listings for a directory target.
  - `host` (`string`, required): Machine name or exact host ID.
  - `idle_ms` (`integer`, optional): Stop run after this long with no connection through Mesh.
  - `isolate` (`boolean`, optional): Send cross-origin isolation headers.
  - `keep_running` (`boolean`, optional): Start run now, never stop it for idleness, and restart it after it exits.
  - `label` (`string`, optional): Display name shown in service lists.
  - `listen` (`array`, optional): PUBLIC=UPSTREAM local port proxies on the host.
  - `ready_timeout_ms` (`integer`, optional): How long a starting run holds connections before failing them (default 60000).
  - `route` (`string`, optional): Route path such as /blog (default from target).
  - `run` (`string`, optional): Command that serves the port; needs a port target.
  - `target` (`string`, required): Port on the host, or a directory on it to serve.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (default 30000).

## mesh_service_start

Start a route that has a run command now and wait until it is ready (default 30 s, up to 120 s). This also ends the stop of a keep-running route.

- Linked operation: `services.start`
- Hints: destructive, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID that owns the route.
  - `name` (`string`, required): Route as mesh_services_list shows it, such as /dev or a service name.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds.

## mesh_service_stop

Stop a route's run command now. Its listeners stay bound. A route with an idle window starts again on its next connection through Mesh; a keep-running route stays stopped until mesh_service_start, a change of its definition, or a daemon restart.

- Linked operation: `services.stop`
- Hints: destructive, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID that owns the route.
  - `name` (`string`, required): Route as mesh_services_list shows it, such as /dev or a service name.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds.

## mesh_service_unpublish

Remove one route from its host, the same as mesh unserve, stopping its run command if it has one. Returns service.not_found only when the host's own route list shows the route absent; unlike mesh unserve --host, which succeeds then. A retry after a lost reply therefore reports service.not_found once the first attempt removed the route. Any other error leaves the route's state unknown; read it with mesh_service_inspect before retrying.

- Linked operation: `services.unpublish`
- Hints: destructive, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID that owns the route.
  - `name` (`string`, required): Route as mesh_services_list shows it, such as /dev or a service name.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds.

## mesh_services_list

List live and cached Mesh serve routes across the fleet or on one host without opening service URLs (which could trigger on-demand startup).

- Linked operation: `services.list`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, optional): Optional machine name or exact host ID filter.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_session_hibernate

Stop a detached Claude or Codex session to free its memory while its conversation stays resumable; the next mesh attach resumes it. The host refuses plain shells and attached sessions. Never attaches.

- Linked operation: `sessions.hibernate`
- Hints: destructive, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID.
  - `session_id` (`string`, required): Canonical Mesh session ID.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (default 12000).

## mesh_session_inspect

Inspect live or saved details of a human terminal session without attaching, stealing, or resizing it. Request preview only when screen lines are needed (capped at 80 by 24).

- Linked operation: `sessions.inspect`
- Hints: read-only, idempotent
- Arguments:
  - `host` (`string`, required): Machine name or exact host ID.
  - `preview` (`boolean`, optional): Include a control-free screen preview capped at 80 by 24.
  - `session_id` (`string`, required): Canonical Mesh session ID.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_session_recover

Start a replacement for an interrupted session (after a reboot) that keeps its previous output, without attaching to it: action command restarts the saved command, shell opens a shell in the saved directory, and agent resumes the saved agent conversation. Returns the new session ID; a person attaches with mesh attach. Application-owned jobs and pipe workers are never recovered this way.

- Linked operation: `sessions.recover`
- Hints: destructive
- Arguments:
  - `action` (`string`, required) (command, shell, agent): What the replacement runs.
  - `host` (`string`, required): Machine name or exact host ID.
  - `session_id` (`string`, required): Canonical ID of the interrupted session.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (default 12000).

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

## mesh_terminal_input

Write up to 1 MiB of keys to a terminal job created by this identity, at an expected decimal byte offset, or look up an earlier write_key acknowledgment when offset is omitted. Send control keys as bytes (for example \r for Enter, \u0003 for ctrl+c, \u0004 for ctrl+d). Exact retries return the original acknowledgment; conflicting keys or stale offsets fail without writing. Returns terminal.attached without writing while a person is attached. Refused for pipe jobs; use mesh_job_input for pipe stdin.

- Linked operation: `sessions.input`
- Hints: destructive
- Arguments:
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `data` (`string`, optional): UTF-8 text bytes to type (mutually exclusive with data_base64).
  - `data_base64` (`string`, optional): Base64-encoded bytes to type (mutually exclusive with data).
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `offset` (`string`, optional): Expected nonnegative decimal byte offset; omit to look up write_key without writing.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).
  - `write_key` (`string`, required): Caller-chosen logical write key.

## mesh_terminal_open

Start a keyed terminal (PTY) job inside a durable execution context (requires mesh mcp --identity NAME), for programs that need a real terminal. Pass exactly one of command (shell string, refused for shell=none) or argv. Returns the opaque mj1 job handle at once; read the screen with mesh_terminal_screen, type with mesh_terminal_input, and read raw output with mesh_job_output stream=pty. Replaying request_key with the same intent returns the same job; after a lost reply use mesh_job_receipt. A person can take the terminal with mesh attach at any time; while they are attached, input and resize return terminal.attached.

- Linked operation: `sessions.create`
- Hints: destructive, open-world
- Arguments:
  - `argv` (`array`, optional): Direct executable and arguments (mutually exclusive with command).
  - `cols` (`integer`, optional): Initial terminal width (default 80).
  - `command` (`string`, optional): Shell command string (mutually exclusive with argv; refused when context shell is none).
  - `context` (`string`, required): Opaque context handle (mc1.<hostId>.<contextId>).
  - `cwd` (`string`, optional): Optional absolute or context-relative working directory for this job only.
  - `env` (`object`, optional): Optional per-job environment overlay.
  - `request_key` (`string`, required): Caller-chosen logical request key for idempotent dispatch and recovery via mesh_job_receipt.
  - `rows` (`integer`, optional): Initial terminal height (default 24).
  - `term` (`string`, optional): TERM value for the program (default xterm-256color).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_terminal_resize

Set the size of a terminal job created by this identity. Returns terminal.attached without resizing while a person is attached, because their terminal decides the size.

- Linked operation: `sessions.resize`
- Hints: destructive, idempotent
- Arguments:
  - `cols` (`integer`, required): Terminal width.
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `rows` (`integer`, required): Terminal height.
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).

## mesh_terminal_screen

Read the rendered screen of a terminal job created by this identity, as plain text lines at most 160 by 24, without resizing it or disturbing anyone attached. attached reports whether a person holds the terminal. After the job ends, returns the last saved screen when one exists.

- Linked operation: `sessions.screen`
- Hints: read-only, idempotent
- Arguments:
  - `cols` (`integer`, optional): Preview width (default 80).
  - `context` (`string`, optional): Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership.
  - `job` (`string`, required): Opaque job handle (mj1.<hostId>.<sessionId>.<instance>).
  - `rows` (`integer`, optional): Preview height (default 24).
  - `timeout_ms` (`integer`, optional): Hard deadline in milliseconds (100 to 60000, default 5000).
