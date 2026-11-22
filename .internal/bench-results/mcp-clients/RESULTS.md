# Real MCP clients against `mesh mcp` (#318)

Run on the PC (Linux, load average 30 to 60 from other work) with Mesh `02700c75`, Claude Code
2.1.322 and Codex CLI 0.167.1, over stdio only. `summary.json` records versions, models, the
two structured client events the proof needs, and the results of `prove.py check`.

## Published evidence

Each `trace-*.jsonl` line is one protocol message between a client and `mesh mcp`
(`dir` `c2s` or `s2c`, `pid` of the server wrapper, `t` seconds, `message`). Before
publication, `harness/redact.py`:

- masks every `account` field, whatever its value;
- replaces the private scratch, harness and binary paths with `$PRIVATE`, home directories
  with `<home>`, and the local user name with `<account>`;
- reduces the `tools/list` catalog to tool names and leaves out the `mesh_skill` text.

Model replies, raw client event streams and server stderr stay in the private scratch root
and are never published.

## Setup

`harness/prove.py run` starts the `mcp_native` integration fixture: a client state directory
and a second daemon with its own state, approved for the client device and reached over its
authenticated WebSocket. Nothing touches the owner's daemon or client configuration. The
clients use the owner's existing logins.

- Claude Code: `claude -p --mcp-config FILE --strict-mcp-config --allowedTools mcp__mesh --tools ""`
  (runs 1 and 2), then a project `.mcp.json` with `--settings '{"enableAllProjectMcpServers":true}'`
  (run 3). Model `claude-sonnet-5-5`.
- Codex: `codex exec --ignore-user-config --ephemeral --sandbox read-only` with
  `-c mcp_servers.mesh.{command,args,env}`, `env` including
  `CODEX_MCP_PROTOCOL_VERSION="2026-07-28"`, `--enable mcp_2026_07_28`, and
  `default_tools_approval_mode="approve"`. Model `gpt-6.1-terra`.

Each server is `mesh mcp --identity NAME` behind `harness/mcp_tee.py`.

## What `prove.py check` requires

| Run | Requirement on the stored evidence |
| --- | --- |
| Every modern run | Each server process starts with a successful `server/discover` listing `2026-07-28`, has a successful `tools/list` that includes `mesh_exec`, sends no `initialize`, and every request carries the protocol metadata and gets exactly one reply |
| Claude Code and Codex run 1 | One successful `mesh_hosts_list`, `mesh_context_open` and `mesh_exec`; `mesh_exec` used the returned context and the expected request key, exited 0 and printed `hello from ...` |
| Claude Code and Codex run 2 (new client, new `mesh mcp`) | No job started; `mesh_job_receipt` used run 1's context and request key and returned run 1's exact job handle; `mesh_job_output` read that job and returned run 1's exact stdout |
| Claude Code run 3 | Server `mesh` connected from the project `.mcp.json`, and `mesh_hosts_list` succeeded |
| Codex with neither opt-in, the feature only, or the env only | Only `initialize` requests (2025-06-18), each answered with `-32601`. Codex went on without mesh tools: unsupported in that mode |
| Codex without the approval setting | `mesh_exec` never reached the server, and Codex reported "MCP tool call requires approval, but approval policy is never" |

Timings in `summary.json` (`callSeconds`) are from a heavily loaded host and are not a
benchmark. The overhead benchmark is in PR 527.

## Reproduce

Check the stored evidence (no model account needed):

```sh
python3 -B .internal/bench-results/mcp-clients/harness/prove.py check .internal/bench-results/mcp-clients
```

Regenerate it with real clients:

```sh
go build -o "$BIN" ./cmd/mesh
root=$(mktemp -d /work/tmp/mcp318-XXXXXX)   # short: Unix socket paths
python3 -B .internal/bench-results/mcp-clients/harness/prove.py run "$BIN" "$root" .internal/bench-results/mcp-clients
```

Both exit non-zero unless every requirement above holds. `run` always tears down the
fixture; delete `$root` afterwards.
