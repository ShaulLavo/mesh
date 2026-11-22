# One MCP entry for native tools and hosted servers

`mesh mcp --profile NAME --identity ID` is one stateless MCP `2026-07-28` stdio
server that offers Mesh's native tools and the hosted MCP servers a profile
selects, from any number of machines. It runs on the client device and sends
every request straight to the machine that owns the service; there is no
central gateway. Hosting and direct connection are in
[Host and connect to MCP servers](hosted-mcp.md).

## Profiles

Profiles live in `mcp-profiles.json` beside `hosts.json`, a private `0600`
file with a revision that every write checks and advances.

```sh
mesh mcp identity create codex
mesh mcp profile add dev --native hosts,exec,files
mesh mcp profile add dev pc/repo-tools --alias repo
mesh mcp profile add dev vps/search --alias search --exclude 'admin_*'
mesh mcp profile show dev --json
```

- `--native` takes `all`, `none`, or a comma list of `hosts`, `contexts`,
  `exec`, `sessions`, `services`, `terminal`, `files` and `fleet`.
  `mesh_skill` is always present.
- `profile add NAME HOST/SERVICE` reads the host's catalog once and stores the
  stable host ID and service ID. `--include` and `--exclude` take shell-style
  patterns on upstream tool and prompt names. `--instance` pins an instance.
- Every write accepts `--expected-revision N`; a stale revision fails with
  `fleet.revision_conflict` and changes nothing.
- `profile show` reads each selected host's catalog passively (it never starts
  a service) and reports each entry with its hosted catalog state (`ready`,
  `stopped`, `starting`, `incompatible`, `failed` and so on) or with
  `offline`, `unauthorized`, `unsupported`, `identity_changed` or
  `not_found`. `--cached` shows the file only.
- `profile alias NAME OLD NEW` renames the display alias. Exposed names do
  not change.
- A profile holds at most 32 services and exposes at most 2,000 hosted tools.

Renaming a host changes nothing, because entries use host IDs. A host whose
identity changed reports `route.identity_changed` until it is enrolled again.
A service that was removed and published again has a new ID, so its entry is
`not_found` until you rebind it by its recorded name:

```sh
mesh mcp profile repair dev repo            # shows the proposed new ID
mesh mcp profile repair dev repo --confirm  # rebinds; its exposed names change
```

## Names and URIs

Native tools keep their `mesh_` names. Hosted tools and prompts are named
`h_<host>_<service>_<kind>_<name>`: the host ID's 32 bytes, the service ID's
16 bytes and the upstream UTF-8 name, each in lowercase unpadded Base32, with
kind `t` for tools and `p` for prompts. The fields cannot contain underscores,
so two upstream names never collide, and the gateway decodes any such name
exactly, even one it never listed. A name longer than 128 characters gets a
numbered `hx_N` name from the registry in `mcp-profiles.json`; an `hx_` number
is never given to another tool.

Hosted resources and templates are
`mesh-mcp://<host>/<service>/<original URI>`. The original URI follows the
fixed prefix byte for byte, percent-encoding and template variables included,
so an expanded template or a URI the client never listed reads correctly. The
gateway rewrites only resource URI fields (lists, reads, resource links,
embedded resources, subscriptions and update notifications); text, structured
content and unknown fields pass through.

Each hosted entry carries
`_meta["com.github.shaullavo.mesh/route"]` with `hostId`, `serviceId`,
`alias`, `revision` and `stale`. Results identify Mesh in
`io.modelcontextprotocol/serverInfo` and the hosted server in
`com.github.shaullavo.mesh/upstreamServerInfo`.

## Lists, caching and refresh

`tools/list` returns native tools first, then each selected service in profile
order with its entries in upstream order, in pages of at most 200. Every list
request and every page rechecks each host's catalog, so a revoked device, a
re-approved grant, a re-pinned host or a removed service takes effect on the
next page. A page cursor is signed with a key derived from the application
identity and binds the profiles file revision and the state each listing was
read under; any change, even an edit that is later reverted, fails it and the
client lists again from the start.

The owning host keeps a listing revision for each service and account. It
advances it when a listing that passes through it differs from the last one,
when an upstream reports a list change, and when the service is published
again, and reports it in `mcp.catalog` and on the `mcp` topic of
`state.watch`. The gateway binds every cached listing to that revision, the
service revision and account, the pinned instance and the caller's scope on
the host (which changes when the device is approved again), and fetches it
again when any of them differs. Listings are cached privately per host pin
and application identity under the state directory for the upstream TTL, at
most 5 minutes; the reply advertises the shortest remaining TTL. When a host
is unreachable, a listing read under the caller's current scope stays usable
for 24 hours with `stale: true` and `ttlMs: 0`, then expires and is never
served. An authorization failure or a changed host identity discards this
caller's listings of that host at once; other callers' records stay.

Each list result reports every selected route under
`_meta["com.github.shaullavo.mesh/routes"]` with its `state` (`ready`,
`stopped`, `stale`, `expired`, `offline`, `unauthorized`, `unsupported`,
`incompatible`, `failed`, `identity_changed` or `not_found`), `freshness`
(`live`, `cached`, `stale` or `expired`) and any error code. Upstream
page-level fields and metadata that cannot be merged are kept per route under
`_meta["com.github.shaullavo.mesh/upstreams"]`. Unowned list request
parameters are passed to each upstream. Resource URIs and templates with
invalid percent escapes, whitespace or unbalanced template expressions are
left out.

Discovery advertises `tools.listChanged` always (profile edits are watched),
`prompts`, `resources` and `completions` when a selected service offers them,
and `resources.subscribe` only when one supports it. Tasks are not offered.

**Refresh.** In a `subscriptions/listen`, list changes come from three
places: the upstream's own listener when it acknowledged the filter, the
owning host's listing revisions on `state.watch` (one watch per host, shared
by the frontend's listens), and edits to the profile. A host that cannot
deliver listing revisions falls back to bounded refresh: the gateway reads a
baseline when the listen starts, re-reads each listing about once a minute
with jitter, backs off to 5 minutes on failure, and reports a changed
listing. Resource list changes also cover resource templates. A client that
does not listen refreshes when the advertised TTL runs out or by listing
again.

## Calls, input rounds and subscriptions

Calls go to the owning host over the same authenticated stream as
`mesh mcp connect`, by exact service ID, carrying the service revision and
account the gateway resolved. A service that was published again since
refuses the call with `catalog.stale` and the gateway retires its cached
listing; a service removed and published again never receives the old ID's
calls, even under a name equal to that ID. A changed instance pin takes
effect on the next request. Unknown names and URIs get `-32602`. Hosted
failures inside `tools/call` are tool execution errors (`isError: true` with
the agent envelope); elsewhere they are `-31000` with `meshCode`. Other
services stay usable. Tool calls, prompt gets, reads and continuations are
never retried automatically, so a write interrupted by a lost link or a
retired grant reports its outcome as unknown and does not run twice.

An `input_required` result passes through with the host's continuation
reference. Send the next round to the same name or URI with that
`requestState` and your `inputResponses`; declare the matching client
capability (for example `elicitation`) on the retry. The host binds each
reference to the application identity, device grant, service revision,
account, instance generation, method and arguments, accepts it once, and
expires it after 10 minutes. It checks everything but the instance before
choosing or starting an instance, so a replayed, altered or obsolete
reference gets `-32602` with `meshCode: mcp.state_invalid` without starting
anything. A new `mesh mcp --profile` process with the
same identity can continue a round started by an earlier one.

`subscriptions/listen` resolves every route and collects the upstream
acknowledgments within one 5-second setup bound, so a silent host cannot
hold the acknowledgment. It acknowledges only what is backed: list-change
filters, plus each resource URI its upstream accepted. A route whose
upstream acknowledges nothing, fails before acknowledging or closes after an
empty acknowledgment is left out and never ends the listen. Events that
arrive before the downstream acknowledgment wait for it. Updates are tagged
with the client's listen ID, keep their own fields and are coalesced within
the frontend's shared byte budget, so a slow client never stalls an
upstream; an update that would exceed the 3.5 MiB message limit after the
URI rewrite ends only its own listen. One frontend holds at most four
listens; a fifth gets `resource.limit`. Cancelling one listen cancels exactly
its upstream listens. When an accepted upstream ends its listen, or a profile
edit changes the routes (including an instance pin), the listen reports the
list changes and ends with one complete result; listen again. A retired
device grant ends it with `access.denied` and drops its events not yet
written.

## Client configuration

Both clients below were run against `mesh mcp --profile` with two hosting
machines: discovery, a listen, every list, native and hosted calls, progress
and three-round input. In their non-interactive modes (`claude -p`,
`codex exec`) the clients answer input requests themselves with `cancel` or
`decline`; interactive sessions ask the user.

Codex 0.167.1 (with its `mcp_2026_07_28` development feature). Codex asks
before calling a tool that is not read-only unless the server's
`default_tools_approval_mode` allows it:

```toml
[features]
mcp_2026_07_28 = true

[mcp_servers.mesh]
command = "mesh"
args = ["mcp", "--profile", "dev", "--identity", "codex"]

[mcp_servers.mesh.env]
CODEX_MCP_PROTOCOL_VERSION = "2026-07-28"
```

Claude Code 2.1.322:

```json
{
  "mcpServers": {
    "mesh": {"command": "mesh", "args": ["mcp", "--profile", "dev", "--identity", "claude"]}
  }
}
```

Hosted services need `--identity`: without one the gateway serves native
tools only and reports every hosted route as `unauthorized`, whatever the
profile says. The gateway serves stdio only. `mesh_services_list` and
`mesh_service_inspect` also show hosted servers to an identified frontend,
and `mesh_service_lease` holds an on-demand instance running for up to 24
hours under a lease key you choose; repeating a call with the same key
renews the same lease.
