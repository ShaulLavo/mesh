# Use Mesh from an agent

## Load the shipped skill

Run `mesh skill show` for the embedded instructions. This command prints the exact bundled Markdown without reading local configuration, creating an identity, or contacting a daemon.

To install the bundle, choose an existing skills root that you own and that nobody else can write to:

```sh
mesh skill install --dir ROOT --json
mesh skill status --dir ROOT --json
```

A root that links to a shared skills directory is supported. Mesh resolves that root once. Links inside its managed `mesh/` directory are refused. The manifest binds an install to its Mesh configuration and state directories. Another configuration cannot update or uninstall it; use the original paths so proposal ownership and registry serialization remain consistent.

To refresh all registered roots, run:

```sh
mesh skill update --json
```

To remove one managed install, run:

```sh
mesh skill uninstall --dir ROOT --json
```

Mesh preserves edited files during update and uninstall. An update reports exact proposal paths in Mesh's state directory when your edits conflict with the bundle. Edited proposals also survive uninstall. Read `registered` and `registrationState` separately from the tree state. If a registry write fails, repeat the same command to repair it.

Mesh manages at most 32 registered roots. It never scans provider settings, changes recovery hooks, creates the selected skills root, or refreshes skills automatically.

## Read machine context

```sh
mesh hosts --json
mesh inspect HOST_ID --json
mesh session list --host HOST_ID --json
mesh session inspect --host HOST_ID SESSION_ID --json
mesh serve ls --json
```

Use the exact returned host ID. Read the full host and fleet notes before acting. Missing, invalid, untrusted, and unavailable notes are distinct states. Notes provide context, not permission. Existing device grants permit full control, and Mesh does not restrict what an agent does with that access.

These commands do not wake a machine, start a daemon, attach to a session, or open a service URL. Opening an on-demand service URL can start that service. Add `--preview` to session inspection only when you need its output. Without that flag, Mesh internally reads one cell and omits preview bytes.

There is no agent-safe execution command in this version. When the user asks for a terminal session, `mesh --host HOST_ID -- CMD` starts persistent work with terminal control codes in its output. Disconnecting leaves the command running. Never take over a session to inspect it or remove a session you did not start.

## Change owner notes

Read the current revision, then replace the notes with an editable JSON file:

```sh
mesh profile show --json
mesh profile set --file PATH --expected-revision N --json
```

Edit fleet notes only when the user asks. Add `--fleet` to the read and write commands. Check delivery with `mesh profile status --fleet --json`. Offline members can remain pending. A revision conflict requires another read before you retry.

## Handle JSON results

Each JSON invocation writes one envelope with `schemaVersion`, `operation`, `status`, `requestId`, `data`, and `errors`. Exit 0 means complete, 3 means partial with useful data, 2 means invalid usage or input, and 1 means operational failure. A session exit code is data, not the CLI exit code. JSON mode cannot be combined with privacy mode.

`mesh skill show` is Markdown-only. Use `mesh skill show --file references/commands.md` for the generated reference of shipped agent CLI operations, `mesh skill show --file references/mcp-tools.md` (or the [MCP tools reference](../reference/mcp-tools.md)) for the native `mesh mcp` stdio server tools, or the [CLI reference](../reference/cli.md) for all commands.

## Run the native MCP stdio server

Create a per-agent application identity once with `mesh mcp identity create NAME`, then start `mesh mcp --identity NAME` from an MCP client that speaks the stateless `2026-07-28` protocol. Without `--identity`, `mesh mcp` serves passive discovery tools and refuses `mesh_context_*` and `mesh_exec` with `usage.invalid`. `stdout` carries only newline-delimited JSON-RPC MCP messages; diagnostics, `--help`, and startup errors go to `stderr` (exiting with status 2 before any protocol output on bad usage). Legacy `initialize` handshakes are refused with `-32601`.

Tool results carry the same agent envelope in `structuredContent` and a bounded summary in `content`. Opaque context handles (`mc1.<hostId>.<contextId>`) and job handles (`mj1.<hostId>.<sessionId>.<instance>`) let a fresh `mesh mcp --identity NAME` process open/update/close working contexts, run keyed pipe commands with `mesh_exec`, or inspect, wait on, read output from, send stdin to, signal, cancel, or remove durable jobs as its very first request without discovery. A retried `mesh_context_open` and `mesh_context_receipt` return the original revision-1 snapshot; call `mesh_context_get` for the current state. Cancelling a wait or closing stdin never cancels an accepted job.

`mesh_terminal_open`, `mesh_terminal_input`, `mesh_terminal_resize` and `mesh_terminal_screen` run terminal jobs in a context; they reach only jobs the identity created, and input and resize return `terminal.attached` while a person is attached. `mesh_service_start`, `mesh_service_stop`, `mesh_service_publish`, `mesh_service_unpublish`, `mesh_apps_list`, `mesh_session_hibernate` and `mesh_session_recover` use the same host controls and restrictions as `mesh serve`, `mesh unserve`, `mesh app list`, `mesh hibernate` and `mesh recover`, without attaching to anything.

## Configure an MCP client

Give each agent client its own identity, so its contexts and jobs stay separate from other agents on the same account:

```sh
mesh mcp identity create claude
mesh mcp identity create codex
```

### Claude Code

Add the server to a project. Claude Code 2.1.322 wrote this entry to the project's `.mcp.json`:

```sh
claude mcp add --scope project mesh -- mesh mcp --identity claude
```

```json
{
  "mcpServers": {
    "mesh": {
      "type": "stdio",
      "command": "mesh",
      "args": ["mcp", "--identity", "claude"],
      "env": {}
    }
  }
}
```

With this stdio entry, Claude Code 2.1.322 sent `server/discover` first and never sent `initialize`. Claude Code's runtime and feature settings choose which protocol it uses ([MCP client runtimes](https://code.claude.com/docs/en/mcp#mcp-client-runtimes)), so another version or setting can try the legacy handshake, which Mesh refuses. For `claude -p`, allow the tools with `--allowedTools mcp__mesh`. A non-interactive run does not prompt to approve project servers: the verified `.mcp.json` run passed `--settings '{"enableAllProjectMcpServers":true}'`.

### Codex CLI

With a stdio server entry, Codex CLI 0.167.1 used the stateless protocol only with two opt-ins: its `mcp_2026_07_28` feature, and `CODEX_MCP_PROTOCOL_VERSION` in that server's `env`. In `~/.codex/config.toml`:

```toml
[features]
mcp_2026_07_28 = true

[mcp_servers.mesh]
command = "mesh"
args = ["mcp", "--identity", "codex"]
env = { CODEX_MCP_PROTOCOL_VERSION = "2026-07-28" }
```

With either opt-in missing, Codex 0.167.1 sent a legacy `initialize`. Mesh refused it, and `codex exec` went on without the mesh tools and printed no error. For runs that depend on Mesh, set `required = true` in `[mcp_servers.mesh]`: the PR review saw Codex 0.167.1 then exit 1 and print Mesh's refusal, using a stub server rather than the runs below. Codex prints an "under-development features" warning while the feature is on. In `codex exec`, Codex refused to send `mesh_exec` ("MCP tool call requires approval, but approval policy is never") unless the server entry also set `default_tools_approval_mode = "approve"`.

### What was verified

Verified on Linux on 2026-11-22 with Mesh at `02700c75`, Claude Code 2.1.322 and Codex CLI 0.167.1, over stdio only, run non-interactively (`claude -p` and `codex exec`) against an isolated second daemon. With the configurations above, each client listed hosts, opened a context and ran a command with `mesh_exec`. A second client process, with a fresh `mesh mcp`, then found the same job by its request key and read its output. Claude Code was configured with `--mcp-config` and, separately, with the project `.mcp.json`. Codex was configured with `-c` overrides that use the same keys as the file above. Codex was also run with neither opt-in and with each one alone, and without the approval setting. Interactive sessions, other client versions and the loopback HTTP endpoint were not tested. The redacted traces, and the script that produced and checks them, are in `.internal/bench-results/mcp-clients/`.

## Run the native MCP loopback HTTP server

When a local MCP client speaks stateless `2026-07-28` over Streamable HTTP, bind the native frontend to one explicit loopback address and application identity:

```sh
mesh mcp identity create codex
mesh mcp --http 127.0.0.1:8765 --identity codex
```

Mesh exposes the same native tool catalog (including contexts and `mesh_exec`) through the shared dispatch core, listening only on the given loopback IP and port at `/mcp`. It matches the exact configured `Host` and port, accepts an absent `Origin` or an explicitly configured exact loopback origin (`--allow-origin http://127.0.0.1:3000`), rejects `GET` and `DELETE` with `405`, and ignores incoming `Mcp-Session-Id` and `Last-Event-ID` headers.

Every `POST /mcp` requires `Authorization: Bearer TOKEN` from the private `0600` token file tied to that listener and identity (path printed on stderr; the token value is never logged). Responses use `application/json` or request-scoped `text/event-stream` (`Accept` must include both). Closing the response stream cancels the in-flight HTTP request and never cancels an accepted Mesh job.

To rotate the bearer token and immediately retire active streams using the old token:

```sh
mesh mcp token rotate --http 127.0.0.1:8765 --identity codex --json
```

Read the updated private token file to configure the client with the new token. Standard HTTP endpoints stay local and private on loopback; do not expose `mesh mcp --http` through `mesh serve` or any public port.

## Host and connect to MCP servers across the fleet

To host third-party stateless `2026-07-28` MCP servers on their owning machine, list them passively across saved hosts with `mesh mcp ls --json`, and connect directly from any approved Mesh device with `mesh mcp connect HOST/NAME --identity NAME`, see the [Hosted MCP guide](hosted-mcp.md).
