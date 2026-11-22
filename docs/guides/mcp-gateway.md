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
order with its entries in upstream order, in pages of at most 200. A page
cursor is signed with a key derived from the application identity and fails
once the profile entry or any listing changes; list again from the start.

The first list for a cold service starts it and reads its discovery and
listings. Listings are cached privately per host pin and application identity
under the state directory for the upstream TTL, at most 5 minutes; the reply
advertises the shortest remaining TTL. When a host is unreachable, its cached
listings stay usable for 24 hours with `stale: true` and `ttlMs: 0`, then
expire. An authorization failure or a changed host identity discards that
host's listings at once.

Discovery advertises `tools.listChanged` always (profile edits are watched),
`prompts`, `resources` and `completions` when a selected service offers them,
and `resources.subscribe` only when one supports it. Tasks are not offered.

**Refresh fallback.** In a `subscriptions/listen`, list changes come from the
upstream listener when the service acknowledged it, and otherwise from
bounded refresh: the gateway re-reads that listing about once a minute with
jitter, backs off to 5 minutes on failure, and sends `list_changed` when the
listing differs. A profile edit also sends `list_changed`. A client that does
not listen refreshes when the advertised TTL runs out or by listing again.

## Calls, input rounds and subscriptions

Calls go to the owning host over the same authenticated stream as
`mesh mcp connect`. Unknown names and URIs get `-32602`; host failures use
`-31000` with `meshCode` and keep other services usable. Tool calls, prompt
gets, reads and continuations are never retried automatically.

An `input_required` result passes through with the host's continuation
reference. Send the next round to the same name or URI with that
`requestState` and your `inputResponses`; declare the matching client
capability (for example `elicitation`) on the retry. The host binds each
reference to the application identity, device grant, service revision,
instance generation, method and arguments, accepts it once, and expires it
after 10 minutes, so a replayed or altered reference gets `-32602` with
`meshCode: mcp.state_invalid`. A new `mesh mcp --profile` process with the
same identity can continue a round started by an earlier one.

`subscriptions/listen` opens one upstream listen per service that can back
the request and acknowledges only what is backed: list-change filters, plus
each resource URI its upstream accepted. Updates are tagged with the client's
listen ID and coalesced, so a slow client never stalls an upstream. One
frontend holds at most four listens; a fifth gets `resource.limit`. Cancelling
one listen cancels exactly its upstream listens. When an upstream ends its
listen, or the profile drops a service the listen holds, the listen ends with
one complete result. A revoked device grant ends it with `access.denied`.

## Client configuration

Codex (with its `mcp_2026_07_28` development feature):

```toml
[features]
mcp_2026_07_28 = true

[mcp_servers.mesh]
command = "mesh"
args = ["mcp", "--profile", "dev", "--identity", "codex"]

[mcp_servers.mesh.env]
CODEX_MCP_PROTOCOL_VERSION = "2026-07-28"
```

Claude Code:

```json
{
  "mcpServers": {
    "mesh": {"command": "mesh", "args": ["mcp", "--profile", "dev", "--identity", "claude"]}
  }
}
```

A profile with hosted services needs `--identity`. The gateway serves stdio
only. `mesh_services_list` and `mesh_service_inspect` also show hosted
servers, and `mesh_service_lease` holds an on-demand instance running for up
to 24 hours.
