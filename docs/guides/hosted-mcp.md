# Host and connect to MCP servers across Mesh

Mesh can run an MCP server on the machine that owns its tools, files, and
credentials, supervise it there, and let any approved Mesh device discover and
use it with only Mesh installed on the client. Every MCP hop uses the stateless
MCP `2026-07-28` protocol: no handshake, no protocol sessions, and no fallback
to older versions.

## Publish a stdio server

```sh
mesh mcp publish repo-tools --cwd /work/app -- ./repo-mcp
mesh mcp ls
mesh mcp show repo-tools
```

Commands, working directories, and secret file paths are resolved on the owning
host. Pass `--host HOST` to publish on another approved host, or run the command
directly on the owning machine. Publishing again with the same name stores a new
revision. Calls already in progress finish on the old process (up to 30 seconds);
new calls start a process at the new revision.

| Flag | Effect |
| --- | --- |
| `--host HOST` | Target host (machine name, exact host ID, or `local`; defaults to `local`). |
| `--cwd DIR` | Working directory on the owning host. Defaults to the daemon account's home directory. |
| `--env NAME=VALUE` | Plain environment variable. Repeat as needed. |
| `--secret-env NAME=/abs/file` | Read `NAME` from a private `0600` file at launch. Needs `--account-mode host-account`. |
| `--shared` | One instance for every authorized caller. Without it, each application caller gets its own. |
| `--kept` | Keep the server running and restart it with backoff. Needs `--shared`. |
| `--idle D` | Stop an on-demand instance after `D` without use. Default is 15 minutes. |
| `--ready-timeout D` | Time allowed for `server/discover` to answer. Default is 30 seconds. |
| `--account-mode none\|host-account` | Whose upstream account the server uses. |
| `--account-label TEXT` | Non-secret name of that account, shown to callers. |
| `--json` | Write one standard Mesh agent envelope. |

## Publish an external loopback HTTP server

```sh
mesh mcp publish search --url http://127.0.0.1:8080/mcp --shared \
  --account-mode host-account --account-label "search bot" \
  --header-secret Authorization=/srv/secrets/search-auth
```

The URL must be `127.0.0.1`, `::1`, or `localhost`. Mesh owns no process for an
external HTTP server: start, stop, and idle expiry open and close the connection,
and `mesh mcp logs` has no process stderr to show. Credential headers are read
from their secret files and sent only to that loopback URL. On a cold direct
tool call, the owning host privately probes `server/discover`, fetches the
server's `tools/list` schema for that account, and mirrors declared `x-mcp-header`
tool parameters into HTTP headers automatically. Mesh-managed HTTP processes
(`--run`) are refused in this release.

## Discover hosted servers across the fleet

```sh
mesh mcp ls
mesh mcp ls --host pc --json
mesh mcp ls --identity codex --json
mesh mcp ls --cached --json
```

`mesh mcp ls` queries all saved hosts concurrently under a 2-second budget
(configurable with `--timeout`) and keeps partial results (exit 3 in `--json`
mode when at least one host returns live or stale metadata while another host
fails). Listing is strictly passive: it never wakes offline hosts and never
starts stopped on-demand services.

Mesh keeps a private, metadata-only catalog cache under its state directory,
scoped by pinned host identity and caller scope (device identity plus
`--identity` application fingerprint, or whole-account administration when
`--identity` is omitted). Secret values, secret paths, and process stderr never
enter the catalog or cache.

`mesh mcp ls` reports host and service states distinctly:

| State | Meaning |
| --- | --- |
| `ready` | The server instance is running and passed the `2026-07-28` `server/discover` probe. |
| `stopped` | The definition is published and has no running instance yet (on-demand or stopped). |
| `starting` | An instance is launching or probing readiness. |
| `incompatible` | The server refused `server/discover` or failed strict `2026-07-28` validation; it is not retried in legacy mode. |
| `failed` | The server exited before readiness (for example exit status 127 for a missing runtime) or broke framing. |
| `uncertain` | Recovery could not confirm whether a previous worker still runs; Mesh refuses a second launch until settled. |
| `stale` | The host is currently unreachable; Mesh is showing private cached metadata less than 24 hours old. |
| `expired` | The host is unreachable and its private cached metadata is older than 24 hours; entries are not advertised as active. |
| `offline` | The host is unreachable and no private cached metadata exists for this caller scope. |
| `unauthorized` | The host rejected this device or caller; any cached entries for that host and caller are discarded immediately. |
| `identity_changed` | The host's pinned Mesh identity no longer matches; cached entries for that host are discarded immediately. |
| `unsupported` | The host is online but runs a Mesh daemon without MCP hosting support. |

## Connect from any Mesh device

Only Mesh is required on the client device—no upstream server package, runtime,
or secret files are installed locally.

1. Create a per-agent application identity on the client device (once per agent):

```sh
mesh mcp identity create codex
```

2. Point the MCP client at `mesh mcp connect HOST/NAME --identity NAME`:

```sh
mesh mcp connect pc/repo-tools --identity codex
```

Pass `--instance HANDLE` (either an opaque `mi1.<hostId>.<serviceId>.<instanceId>`
handle or an instance ID) to reconnect to a specific authorized instance.

`mesh mcp connect` runs a stateless MCP `2026-07-28` stdio server on the client:

- **Protocol-only stdout**: `stdout` carries only newline-delimited JSON-RPC MCP
  messages. Help text, diagnostics, and startup errors go to `stderr` (exiting
  with status 2 before any protocol output on bad usage or `--privacy`).
- **Unqualified names and URIs**: Tool, prompt, and resource names pass through
  unmodified over an authenticated, destination-pinned H3 (`mcp.forward`) stream
  directly to the owning host.
- **Fresh direct tool calls**: A fresh process can send `tools/call` as its very
  first request without calling `server/discover` or `tools/list` first.
- **Preserved upstream identity**: Every forwarded result identifies Mesh in
  `_meta["io.modelcontextprotocol/serverInfo"]` and preserves the hosted server's
  own identity in `_meta["com.github.shaullavo.mesh/upstreamServerInfo"]`.
- **Cancellation and retries**: `notifications/cancelled` on stdio immediately
  closes the H3 stream and cancels the exact upstream request on the owning host.
  Closing stdin drains and exits within 2 seconds. Read-only discovery and
  unconditional list requests retry once after a transport failure; tool calls,
  prompt gets, resource reads, and multi-round continuations are never retried
  automatically and report `request.outcome_unknown` if interrupted.

### Client configuration examples

Clients must speak stateless MCP `2026-07-28`; legacy `initialize` handshakes
are refused with JSON-RPC `-32601`.

**Codex CLI** (with the `mcp_2026_07_28` development feature enabled and stdio
modern mode opted in):

```toml
[features]
mcp_2026_07_28 = true

[mcp_servers.repo_tools]
command = "mesh"
args = ["mcp", "connect", "pc/repo-tools", "--identity", "codex"]

[mcp_servers.repo_tools.env]
CODEX_MCP_PROTOCOL_VERSION = "2026-07-28"
```

**Claude Code** (modern v2 stateless stdio server entry):

```json
{
  "mcpServers": {
    "repo-tools": {
      "command": "mesh",
      "args": ["mcp", "connect", "pc/repo-tools", "--identity", "claude"]
    }
  }
}
```

## Manage instances and diagnostics

All management commands accept `NAME` (for this machine) or `HOST/NAME` (for any
approved host), plus `--json` for the standard agent envelope:

```sh
mesh mcp show pc/repo-tools --json
mesh mcp start pc/repo-tools
mesh mcp stop pc/repo-tools
mesh mcp restart pc/repo-tools
mesh mcp logs pc/repo-tools
mesh mcp rm pc/repo-tools
```

An isolated service gives each application caller (`--identity NAME` on a device)
its own process. Two agents on the same device or two different Mesh devices get
separate instances. With `--shared`, every authorized caller shares one process
while request IDs, progress tokens, subscriptions, and multi-round input
continuations stay separate per caller.

`mesh mcp logs` reads the instance's private 1 MiB stderr ring and requires
access to that instance.

## Survival table

| Event | Managed instance | In-flight request | Persisted definition |
| --- | --- | --- | --- |
| Client disconnect | Keeps running while eligible | Cancelled upstream | Kept |
| Daemon restart or upgrade | Worker may survive and is adopted by exact reservation | Non-list calls report `request.outcome_unknown`; no replay | Kept |
| Instance crash or stop | Process state lost (kept services restart with backoff) | In-flight calls fail; no replay | Kept |
| Host reboot | Process state lost and kept services start again | Lost | Kept |

If recovery cannot determine whether a previous worker still exists, the
instance is marked `uncertain` and Mesh refuses to start a second worker until
ownership is established.

## Security boundary and known limits

- **Secret files**: A secret file must be a regular file owned by the daemon
  account with mode `0600` or stricter. Mesh opens it without following symlinks
  and reads it only at launch or dispatch.
- **Account isolation**: Shared host credentials are an explicit per-service
  policy (`--account-mode host-account --account-label TEXT`). Services that
  require per-user OAuth (`--account-mode per-user`) are refused: Mesh never
  substitutes a host token for a user's own account.
- **Host OS boundary**: Mesh isolates separately configured application callers
  on application control paths and enforces device grants on every dispatch and
  delivery. Existing whole-account administration and OS account privileges
  define the surrounding trust boundary: hosted processes run as the daemon
  account without a sandbox, so anything else running as that OS account can
  inspect their environment.
- **Non-durable hosted calls**: Hosted calls are not durable; an interrupted
  mutation reports `request.outcome_unknown` and is never replayed automatically.
- **Message size cap**: MCP messages over 3.5 MiB (or H3 control frames over
  4 MiB) are refused with `mcp.message_too_large`.
- **Client protocol requirement**: Legacy-only MCP clients cannot use `mesh mcp`
  or `mesh mcp connect`. Codex requires its `mcp_2026_07_28` feature flag and
  `CODEX_MCP_PROTOCOL_VERSION=2026-07-28` until enabled by default.
