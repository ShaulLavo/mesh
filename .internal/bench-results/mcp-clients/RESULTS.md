# Real MCP clients against `mesh mcp` (#318)

Run on 2026-11-22 on the PC (Linux, load average 30 to 60 from other work), Mesh `02700c75`
(main after PR 528), transport stdio. `summary.json` is the reproducer's own output; the
`trace-*.jsonl` files hold every stdio line in both directions, with local paths replaced by
`$ROOT`/`$SCRATCH`, the account name by `<account>`, the 44-tool catalog reduced to its tool
names, and the `mesh_skill` text left out.

## Setup

`harness/prove.py` starts the `mcp_native` integration fixture: a client state directory and
a second daemon with its own state, approved for the client device and reached over its
authenticated WebSocket. Nothing touches the owner's daemon or client configuration. The
clients use the owner's existing logins.

- Claude Code 2.1.322: `claude -p --mcp-config FILE --strict-mcp-config --allowedTools mcp__mesh --tools ""`
  (runs 1 and 2), then a project `.mcp.json` with `--settings '{"enableAllProjectMcpServers":true}'` (run 3).
  Model `claude-sonnet-5-5`.
- Codex CLI 0.167.1: `codex exec --ignore-user-config --ephemeral --sandbox read-only` with
  `-c mcp_servers.mesh.{command,args,env}`, `env` including
  `CODEX_MCP_PROTOCOL_VERSION="2026-07-28"`, `--enable mcp_2026_07_28`, and
  `default_tools_approval_mode="approve"`. Model `gpt-6.1-terra`.

Each server is `mesh mcp --identity NAME` behind `harness/mcp_tee.py`.

## Results

| Client and mode | First request | Calls | Outcome |
| --- | --- | --- | --- |
| Claude Code run 1 | `server/discover` (protocol `2026-07-28`), then `tools/list` | `mesh_hosts_list`, `mesh_context_open`, `mesh_exec` | Command ran on the second daemon and exited 0 with the expected stdout |
| Claude Code run 2 (new client, new `mesh mcp`) | `server/discover` | `mesh_job_receipt` by context and request key, `mesh_job_output` | Same job handle, exit 0, same stdout; no new job |
| Claude Code run 3, project `.mcp.json` | `server/discover` | `mesh_hosts_list` | Server `connected`, source `project` |
| Codex run 1, both opt-ins | `server/discover`, then `tools/list` | `mesh_hosts_list`, `mesh_skill`, `mesh_context_open`, `mesh_exec` | Exit 0 with the expected stdout |
| Codex run 2 (new client, new `mesh mcp`) | `server/discover` | `mesh_skill`, `mesh_job_receipt`, `mesh_job_output` | Same job handle, exit 0, same stdout; no new job |
| Codex, no opt-in | `initialize` (`2025-06-18`), twice | none | Mesh answered `-32601`; Codex continued without mesh tools and reported no error |
| Codex, feature only | `initialize`, twice | none | Same: unsupported |
| Codex, env only | `initialize`, twice | none | Same: unsupported |
| Codex, both opt-ins, no approval mode | `server/discover` | `mesh_exec` refused by Codex | "MCP tool call requires approval, but approval policy is never" |

Neither client sent `initialize` once it had a modern `server/discover` result. Both
validated against Mesh's strict stateless wire: every request carried
`io.modelcontextprotocol/protocolVersion` `2026-07-28` metadata and was answered.

Timings (`summary.json`, `callSeconds`) are from a heavily loaded host and are not a
benchmark: `mesh_exec` of a trivial command took about 10 s, most of it the login shell and
worker launch under load. The overhead benchmark is in PR 527.

## Reproduce

```sh
go build -o "$BIN" ./cmd/mesh
root=$(mktemp -d /work/tmp/mcp318-XXXXXX)   # short: Unix socket paths
python3 -B .internal/bench-results/mcp-clients/harness/prove.py "$BIN" "$root" .internal/bench-results/mcp-clients
```

It exits non-zero unless every outcome above holds, and always tears down the fixture.
