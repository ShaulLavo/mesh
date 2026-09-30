# Agent interface, host context, and bundled skill

Status: proposed implementation plan; no runtime behavior is added by this document.
Reviewed against `main` at `c96fa0af79df62a5ef616a43309af73683a6c337` on 2026-09-30.

## Outcome

An agent should be able to discover a suitable Mesh host, read the owner's
operational context, inspect existing work, and start or observe a persistent
session without driving a picker or taking over a human's terminal.

Mesh remains a session system. Machine context helps a caller choose and use a
host; it does not turn Mesh into a scheduler, deployment product, configuration
manager, or coding agent. One bundled skill teaches the workflow. Live Mesh
queries provide the user's machine-specific information.

Example: a host's observed platform is Linux/arm64, while its owner-authored
profile says it is the home router and must not be rebooted or used for builds.
Those are different kinds of information. Neither is an authorization grant.

This plan has two delivery tracks:

- **Core:** host profiles, passive discovery, structured session operations,
  restart-safe creation receipts, and an installable generic skill.
- **Scoped access:** separately reviewed, opt-in authentication and authorization
  for agent credentials. Core must not advertise this protection before it exists.

All commands, packages, fields, limits, and tests described as additions below
are proposed. Existing behavior is identified explicitly in the baseline.

## 1. Repository baseline and constraints

| Existing surface | Reuse or constraint |
| --- | --- |
| [`internal/cli/config.go`](../../internal/cli/config.go) | `HostRecord` already owns alias, stable ID, pinned Mesh identity, and connection information. `hosts.json` is versioned and rejects unknown fields. Do not create a second address book or put profiles into this file. |
| [`internal/cli/catalog.go`](../../internal/cli/catalog.go) | Concurrent host queries already fall back to cached session rows and expose stale/error state. Extend this pattern, not the meaning of a cached observation. |
| [`internal/cli/command.go`](../../internal/cli/command.go) | Human CLI routing, session inspection, and create/attach boundaries already exist. Bare names can be hosts or sessions; adding commands can collide with saved aliases. |
| [`internal/protocol/control.go`](../../internal/protocol/control.go) | `host.info`, session list/create/inspect/logs, lifecycle controls, services, stable error codes, and bounded log tails already exist. Extend the shared protocol rather than opening another control server. |
| [`internal/daemon/lifecycle.go`](../../internal/daemon/lifecycle.go) | Session creation already deduplicates request IDs in an in-memory map. This is not a durable receipt across daemon restarts. Existing log reads are bounded tails, not a cursor API. |
| [`cmd/mesh/main.go`](../../cmd/mesh/main.go) | Selected agent/update commands bypass Fang presentation. New JSON paths need equivalent guarantees, including failures before command execution. |
| [`internal/cli/agent_setup.go`](../../internal/cli/agent_setup.go) | `mesh agent setup` manages native conversation-recovery hooks. Do not repurpose it to mean operational skill installation. |
| [`CLAUDE.md`](../../CLAUDE.md), [overview](00-overview.md), [decisions](01-decisions.md) | Workers own PTYs, sessions belong to hosts, and the daemon protocol is the extension point. D23 delegates direct-listener admission to Tailscale; destination identity pinning is not client authentication. |

Preserve these invariants throughout implementation:

1. A disconnected client, expired API request, or restarted daemon does not kill a
   running session. A reboot is reported honestly; RAM is not resurrected.
2. Terminal traffic remains direct to the owning host. No central fleet database,
   coordinator, or new relay is introduced.
3. Read operations never attach, steal, resize, inject input, wake a host, start a
   stopped daemon, fetch a served URL, or install software. Updating a local cache
   is allowed; changing the target is not.
4. Existing human CLI, SSH, recovery, hibernation, serving, and update behavior
   remains compatible unless a separate explicit decision changes it.
5. New functionality is generic: no hardcoded personal domains, machine names,
   agent provider, project layout, or assumed installed toolchain.

## 2. Architecture and ownership

```text
Bundled Mesh skill, identical for every installation
                   |
                   v
Versioned CLI results and operation reference
                   |
                   v
Existing Mesh daemon protocol
       |                     |
Host-owned context     Existing sessions/workers
       |
Local client cache, keyed by pinned host identity
```

Keep three sections separate in every context response:

- `observed`: measured platform, reachability, and advertised runtime features.
- `declared`: owner-authored description, labels, warnings, and instructions.
- `access`: the actual admission/enforcement mode and, only when authenticated
  scoped access exists, the caller's effective permissions.

A label such as `role=router` is data, not an implicit denial rule. A note saying
Docker is installed is a declaration, not a successful live probe. Inspection
must not execute owner-supplied probe scripts or enumerate arbitrary credentials,
process environments, package inventories, or home-directory contents.

### 2.1 Host profile

Use JSON initially to match existing configuration conventions and avoid a new
runtime parser dependency. Editing through a temporary human-friendly document
is a CLI concern; it does not require another authoritative representation.

Example editable input:

```json
{
  "schemaVersion": 1,
  "description": "Home router and network services",
  "labels": { "role": "router" },
  "warnings": ["Do not reboot this machine or restart networking."],
  "instructions": "Do not run builds or experiments here. Use the workstation for compute-heavy work."
}
```

The persisted envelope adds `hostId`, `revision`, and `updatedAt`. Callers cannot
choose those fields in editable input. Revision zero means no authored profile;
creation produces revision one. Even an empty replacement increments revision so
clients can distinguish deliberate clearing from unavailable data.

Store the authoritative `profile.json` beside the existing local Mesh config,
using `ConfigPath()` and its environment overrides rather than inventing another
home-directory convention. It describes this installation's own stable identity,
not all peers. Initial writes are **local only**:

```bash
mesh profile show --json
mesh profile set --file profile.json --expected-revision 0 --json
mesh profile edit
```

`edit` reads a revision, opens a temporary file, validates the result, and performs
the same compare-and-swap as `set`. It is human-only and never called by the skill.
Reject concurrent edits with `profile.conflict`; do not silently choose a winner.
Use a cross-process lock, a same-directory temporary file, file synchronization,
atomic replacement, and the repository's durability conventions. Create private
files/directories, reject unsafe symlink targets, and retain the previous valid
profile when validation or publication fails.

Initial limits: 32 KiB encoded profile, 1 KiB description, 16 KiB instructions,
32 labels with 64-byte keys and 256-byte values, and 16 warnings of at most 512
bytes each. Reject duplicate JSON keys, unknown fields, invalid UTF-8, invalid
schema versions, and disallowed control characters. Do not truncate accepted
warnings or instructions. Markdown in instructions remains text, never a command,
include directive, or URL that Mesh automatically fetches.

An absent profile, an invalid profile, an unsupported daemon, and an offline host
are distinct states. Invalid context must not make otherwise readable session
metadata disappear. It must be visible to the caller and prevent the skill from
assuming that it has read the owner's complete instructions.

Profiles are operational notes, not secret storage. In legacy Tailnet mode they
have the same practical trust boundary as the host account and control listener.
Local-only editing does not make them tamper-proof against a caller that can
already execute arbitrary commands as that account.

### 2.2 Discovery, observations, and caching

Add `mesh hosts --json` for a compact selection view and `mesh inspect HOST --json`
for full host context. The compact view includes description, labels, complete
warnings, profile status/revision, and an indication that full instructions must
be read. It does not silently summarize away safety-relevant notes.

Start with cheap, well-defined observations: platform/architecture of the running
daemon, host identity, Mesh build, advertised protocol features, and reachability
from this client. Do not infer GUI availability, tool installation, or task
suitability from the OS. More observations require named, bounded collectors in a
later change; no arbitrary executable probes in this work.

Each independently fetched section carries its source, client `receivedAt`, any
host `observedAt`, and freshness/status. Host wall clocks are not trusted for
cross-machine ordering or authorization. Use profile revisions for update order
and client receipt time for cache freshness.

Cache host context separately from `hosts.json`, following the existing catalog
cache boundary. Cache keys include stable host ID and pinned identity, not alias,
IP, or a raw filename supplied by a peer. Alias changes preserve context; an
identity change requires explicit re-adoption and cannot inherit the old profile
silently. Reject profile revision regression for the same identity and report it
rather than erasing a newer known warning.

`hosts` queries the bounded saved host set concurrently with a two-second default
budget. `inspect` has a five-second default budget. Both accept finite `--timeout`
and `--cached`; cached-only mode makes no network calls. Partial responses retain
successful sections and mark failures individually. Unreachable does not mean
asleep, and a last-known capability is not proof that it is currently available.

Treat the local host deliberately: D29 keeps it outside its own address book.
Include it without adding a fake `HostRecord`, creating an identity during a read,
or delaying local terminal-window startup. Missing local initialization is a
reported state, not permission to bootstrap it.

### 2.3 Access information must tell the truth

Before the scoped-access track lands, return an explicit legacy description:

```json
{
  "mode": "tailnet",
  "principal": null,
  "effectivePermissions": null,
  "enforcement": "tailnet-admission-only"
}
```

Do not turn a profile warning into a fabricated `denied` permission, report an
unauthenticated request as the owner, or treat a successful identity-pin check as
proof of the caller's identity. Supported operations and allowed operations are
separate fields. Cached access information is never an authorization decision.

## 3. CLI and protocol contract

The following is the proposed public surface, not a list of already shipped
commands. Session operations require an explicit `--host` and can pin the resolved
target with `--expect-host-id`; the skill always supplies the pin after discovery.
The daemon must validate the expected identity before any mutation.

| Surface | Behavior |
| --- | --- |
| `mesh hosts --json` | Local host plus saved peers, selection context, freshness, and per-host errors. |
| `mesh inspect HOST --json` | Full observed/declared/access context for exactly one host. |
| `mesh profile show|set|edit` | Local profile administration; `edit` is interactive, others support JSON. |
| `mesh session list --host HOST --json` | Bounded, pageable session summaries; no terminal output by default. |
| `mesh session inspect --host HOST ID --json` | Exact durable state plus available live inspection; preview requires an explicit flag. Never attaches. |
| `mesh session create --host HOST --request-key KEY --cwd PATH --json -- CMD...` | Create a persistent PTY session and return a durable receipt without attaching. |
| `mesh session request --host HOST --request-key KEY --json` | Resolve a creation receipt or a known uncertain outcome after a lost response. |
| `mesh session logs --host HOST ID --cursor CURSOR --max-bytes N --json` | Finite page of combined PTY output with an exact continuation cursor. |
| `mesh session signal|kill --host HOST ID --json` | Explicit lifecycle mutation using existing semantics; no implicit creation, resume, or takeover. |
| `mesh skill show|install|update|status|uninstall` | Inspect or explicitly manage the bundled operational skill, separately from recovery hooks. |

No new attach, terminal input-injection, bulk kill, environment-copying, arbitrary
remote file-write, or remote profile-write API is included in core. Existing
human commands still exist. The skill does not fall back to interactive commands
or raw SSH to bypass an unavailable or denied operation.

### 3.1 Structured result rules

Define typed version-one result DTOs and a small operation registry, shared by
validation, documentation generation, and conformance tests. Do not build a generic
RPC framework or a second model of session state.

JSON output is one envelope containing `schemaVersion`, `status`, `requestId`,
`data`, and `errors`. `status` is `ok`, `partial`, or `error`; every error contains a
stable `code`, human `message`, affected target/operation, and `retryable` boolean.
Request IDs correlate calls. Creation request keys identify logical side effects;
these must not be conflated.

Use exit status 0 for a complete API result, 1 for operational failure, 2 for
invalid invocation, and 3 for useful partial results. An observed session's exit
code belongs in `data`; a completed job exiting 7 does not make a successful
inspection command exit 7. Existing human CLI exit behavior remains unchanged.

JSON mode has no ANSI, update notices, spinners, picker, pager, or prompts.
Diagnostics go to stderr without duplicating the JSON result. Cover Cobra parse
errors and Fang handling, not just successful `RunE` paths. Respect `--` so a
remote program's flags cannot accidentally select local JSON behavior.

Initial stable errors include `host.unreachable`, `host.identity_mismatch`,
`protocol.unsupported`, `profile.invalid`, `profile.conflict`,
`request.key_conflict`, `request.in_progress`, `request.outcome_unknown`,
`logs.cursor_expired`, `access.denied`, and `resource.limit`. Unsupported and
unknown capabilities must not collapse into false or permission denied.

Use the existing control framing and transports. Add explicitly negotiated
controls for full host context, cursor logs, and durable creation. In particular,
do not attach a new key to legacy `session.create` and assume an old daemon will
honor it. An old peer must return unsupported without launching anything. Freeze
feature names and wire fixtures in the first milestone; do not infer support only
from release version strings. A preserved worker can be older than its daemon.

### 3.2 Command-name compatibility

New names such as `hosts`, `inspect`, `profile`, `session`, and `skill` can already
be saved host aliases. Simply adding them to `reservedAliases` would also make
existing `hosts.json` files fail validation.

Before registering new commands, add the explicit human escape form
`mesh --host ALIAS [-- CMD...]`. Separate validation of existing saved records
from validation of newly created/renamed aliases. Continue loading historical
aliases, reserve the new names only for new records, and provide a clear migration
diagnostic without automatically renaming anything. Explicit `--host` must always
reach the saved host, including one named `session` or `inspect`. Do not change
ordinary non-conflicting bare-host/session behavior.

### 3.3 Session creation and retry semantics

Creation is non-interactive but still a Mesh PTY session. Its command may rely on
a terminal, and its stdout/stderr are combined by that PTY; do not promise separate
pipe streams. Use explicit defaults of 80 columns, 24 rows, and `TERM=dumb`, with
bounded optional geometry/terminal overrides. Require an absolute target `cwd`
and a non-empty argv. Do not interpolate through a shell unless the caller
explicitly launches one. The existing host execution environment is used; this
API does not copy the client's environment.

A required opaque `--request-key` is scoped to target identity and, once available,
the authenticated principal. Legacy-mode receipts must identify that weaker
scope honestly. The server computes a digest of the complete normalized request,
including all launch-affecting options. Reusing a key with different options
fails before launch; retrying it with the same options resolves the original
result, even after a daemon restart.

Extend the existing creation path instead of implementing another worker launcher:

1. Atomically reserve a request record and session ID in the owning host's durable
   storage before starting a process. Add an additive migration and storage
   methods; choose the next free migration number at implementation time.
2. Persist launch intent and carry the reserved identity/request fingerprint into
   worker startup. Serialize reservation ownership across processes. Reuse the
   recovery subsystem's durable-reservation concepts where appropriate, without
   changing what a recovery action means.
3. Start the detached worker, publish its metadata, and persist the receipt.
   A dropped response is not grounds for another launch.
4. On daemon startup, reconcile pending records with preserved workers and durable
   metadata before serving retries. Return the same session when proven. If a
   crash boundary makes execution impossible to determine, record
   `request.outcome_unknown` and require explicit operator resolution; never
   silently rerun an arbitrary command.

This is not a claim of exactly-once external side effects after arbitrary power
loss. The safety rule is to prefer a visible uncertain outcome over duplicate
execution. Do not automatically relaunch an interrupted or completed session when
its creation key is retried.

Retain active and uncertain receipts. Retain terminal receipts for at least 30
days and advertise their retention boundary in results. Bound the journal to
10,000 records initially; reject new work with `resource.limit` rather than evict
an unexpired receipt. Purge only eligible terminal records through explicit
maintenance. Document that automatic retries are not safe beyond the advertised
retention window. No background retry loop may switch to a new key.

Keep logical command lifetime separate from API deadlines. Cancelling the client
or timing out while awaiting a receipt stops waiting, not the accepted session.
Creation returns IDs/state, never enters attach automatically, and leaves recovery
and hibernation transitions explicit.

### 3.4 Session reads and output

Expose existing inspection without recreating its process/terminal observation
logic. Report durable state independently from live inspection availability. A
hibernated session stays hibernated when inspected; a legacy worker's missing
preview is unavailable, not a reason to attach.

List results are bounded and pageable. Default summaries omit full command lines,
working directories, and terminal output; explicit session inspection can request
operational detail. Host inspection does not fetch terminal contents. Logs and
previews may contain secrets even when they are read-only; do not promise perfect
redaction of arbitrary program output.

Build log cursors on D1's absolute byte offsets and bind them to host, session,
and output generation/attempt. Cursors are opaque strings, avoiding JavaScript
integer precision loss and accidental use against another session. Use 64 KiB
pages by default and retain the existing 1 MiB upper bound. Preserve byte-exact
output in an encoded field; any human/plain-text rendering must escape terminal
controls and remain separate from cursor accounting.

Return `nextCursor`, completion state, and explicit retained-range information.
An expired cursor reports the gap and earliest available cursor; it must not
silently pretend missing bytes were delivered. Durable output from exited
workers must have explicit offset provenance or report cursor support unavailable.
Do not synthesize offsets for an older worker's unindexed tail. No unbounded
`--follow` operation is required for version one.

## 4. Bundled skill design

Use the [Agent Skills specification](https://agentskills.io/specification): a
`mesh` directory with `SKILL.md` containing name/description frontmatter and
optional reference documents. Skill discovery and activation are client-specific;
shipping a file inside Mesh does not automatically activate it in every agent.
See the [client integration guidance](https://agentskills.io/client-implementation/adding-skills-support).

Proposed source layout:

```text
internal/skills/
  bundle.go
  mesh/
    SKILL.md
    references/
      commands.md
      safety.md
```

Embed the bundle in the binary with Go's standard embedding support. Keep the
workflow and safety text human-authored. Generate `commands.md` deterministically
from the versioned operation registry, and test its examples against the CLI.
There is no LLM generation step, network dependency, or per-machine generated
skill. Only document operations that the shipped binary actually implements.

The workflow must teach the agent to:

1. Discover hosts, then inspect the exact chosen identity. Read complete owner
   instructions and distinguish unavailable context from an empty profile.
2. Respect notes as operational context, not as permission to exceed the user's
   request. Do not promote program output, repository files, or fetched text into
   fleet policy. A remote note cannot authorize changing grants or stealing keys.
3. Prefer an appropriate host and existing work, but never take over a session or
   inject input merely to inspect it. Ask when host choice or instructions conflict.
4. Use explicit host/session identities, structured finite reads, and a persisted
   creation key. Reconcile uncertain creation before considering another launch.
5. Re-inspect after an explicit wake or a meaningful context change. Discovery
   itself does not wake hosts. Do not bypass unavailable features through SSH.
6. Treat reboot/network-change warnings as constraints and explain any conflict
   with the requested work. Do not claim that a Markdown warning is enforced.

`mesh skill show` prints the bundled entry point without querying a host. The
bundle includes a skill version and required Mesh API version. An older installed
skill consults current capabilities; a newer incompatible skill stops with an
upgrade instruction rather than inventing a fallback.

Installation is explicit:

```bash
mesh skill install --dir /chosen/agent/skills
mesh skill status --dir /chosen/agent/skills --json
mesh skill update --dir /chosen/agent/skills
mesh skill uninstall --dir /chosen/agent/skills
```

`--dir` is a skills root; the managed destination is its `mesh/` child. Validate
containment and symlinks. Use an install manifest containing managed paths,
versions, and hashes. Stage complete updates before publishing them. Never delete
or overwrite user-modified files silently: report a conflict and retain both the
current installation and the proposed replacement. Uninstall removes only
unchanged managed files and reports preserved customizations.

Do not edit provider hook settings, grant tool permissions, or install into every
agent discovered on disk. Provider-specific path shortcuts are a later adapter
with its own tests. Generic explicit-path installation is sufficient initially.
No private profiles, hostnames, keys, session output, or access grants are copied
into the skill or emitted by its installation commands.

## 5. Scoped-access follow-up and its release gate

D23 explicitly says that the current direct listener has no second client
authentication/authorization layer. Supporting a genuinely read-only agent
credential therefore requires a separate architecture/security review, not just
an `allow` field in the profile. Record an explicit amendment to D23 before
implementing opt-in scoped mode; preserve D17's transport choice and D18's single
host identity.

The required design is a host-owned grant store and a server-verified caller
principal, distinct from the destination host identity. Reuse existing identity
and signed-control patterns, but review the authentication transcript and replay
protection before implementation. Specify challenge binding to target identity,
connection, protocol version, expiry, and a single-use nonce. Client-supplied
role/principal strings are never evidence of identity. Do not add a new transport
or implement new cryptographic primitives.

Scoped mode must fail closed on unknown/revoked credentials and on legacy
unauthenticated control requests. It cannot coexist with an unrestricted route
to the same protected control operations. Legacy Tailnet-only mode remains an
explicit compatibility choice, never a silent fallback for a scoped credential.

Start with explicit operation permissions, not an inference from machine labels:
metadata/context read, session detail read, output read, session create, session
mutation, and administrative operations. Logs/previews need their own permission
because read-only output can still disclose secrets. Protect profile/grant edits
separately; an execution agent does not get them by default.

Authorization must cover the actual dispatch/relay paths, not just new CLI
commands. Audit direct WebSocket controls, SSH commands and channels, session
attach/input/resize, recovery and resume, update/bootstrap operations, serving
controls, and other paths capable of starting work. Check current grants at the
moment of each effect. Revoke active control access without killing the detached
session. Missing policy support in an older daemon/worker must never become an
allow decision.

Local same-user worker sockets and arbitrary shell access are part of the OS
trust boundary, not a sandbox. An agent with the owner's account, administrator
credentials, or arbitrary execution as that account can bypass text guidance and
may bypass Mesh-only restrictions. Document this plainly. Public/private serving
routes with intentional on-demand behavior also need an explicit threat-model
boundary; inspecting a service is not permission to fetch its URL and start it.

No reboot-management feature is added here. The router example is satisfied first
by owner context; strong isolation requires appropriate credentials and OS/network
permissions. A `--yes` flag or submitted profile revision is not independent human
approval.

This track is not required to ship advisory profiles and a skill. It is required
before advertising per-agent enforced permissions, enabling remote profile/grant
administration, or describing an agent key as securely read-only.

## 6. Implementation sequence

The identifiers below are local to this plan. They do not reserve or renumber the
repository's existing `Txx` tasks. Each milestone should be a separately reviewable
implementation PR with tests; this planning PR adds only this document.

### AI-01: Freeze contracts and preserve CLI compatibility

Dependencies: none.

Touch `internal/protocol`, `internal/cli/command.go`, `internal/cli/config.go`,
`cmd/mesh/main.go`, and a small new `internal/agentapi` package for result DTOs and
operation metadata. Keep session state types shared with the existing protocol.

Deliver version-one JSON schemas/fixtures, error and exit-code tables, finite
limits, protocol feature names, and the explicit `--host` escape. Implement the
split between loading historical aliases and validating new aliases before
registering the new commands. Add tests for JSON parsing/validation failures,
stdout cleanliness, and an old saved host named after each new command.

Acceptance: existing non-conflicting CLI behavior is unchanged; old address books
still load; explicit host selection is unambiguous; unsupported new controls have
no side effects. Schema examples and operation-reference metadata are tested.

### AI-02: Host-owned profiles and bounded context cache

Dependencies: AI-01.

Add `internal/hostprofile` for validation, revisions, and atomic local storage;
use `internal/paths`/existing config resolution. Extend the client cache behind
`internal/cli/catalog.go` or an adjacent adapter rather than changing `HostRecord`.
Add the local profile commands and read-only host-context protocol handler.

Acceptance: exact round trip of multiline instructions; absent/invalid/offline
states distinguishable; compare-and-swap survives concurrent writers; crash or
disk-full during publication preserves the previous profile; aliases do not own
cache identity; inspection never runs profile content or exposes unrelated files.
Test config overrides, symlink rejection, oversized/malformed JSON, profile
clearing, identity replacement, and revision regression.

### AI-03: Passive agent discovery and session reads

Dependencies: AI-01 and AI-02.

Wire `hosts`, `inspect`, session list, and session inspect through
`internal/cli/catalog.go`, the existing CLI inspection boundary,
`internal/daemon/lifecycle.go`, `internal/protocol`, and worker capability checks.
Add pagination and explicit per-section freshness. Cursor logs may land with
AI-05; do not advertise them before both live and durable-output paths conform.

Acceptance: local results do not require network discovery; one unreachable peer
produces bounded partial output; all failures are structured; a busy terminal
keeps its attacher and geometry; hibernated sessions remain stopped; no read wakes
hosts, launches daemons, fetches services, or starts on-demand commands. Test old
workers under new daemons as well as fully old peers.

### AI-04: Bundle and explicitly install the operational skill

Dependencies: AI-03 for the initial read-only skill; expand only as later commands
land.

Add the embedded `internal/skills` bundle, deterministic command-reference
generation, install manifest handling, and `mesh skill` commands. Do not modify
`mesh agent setup` or provider recovery hooks. Add human documentation explaining
skill installation versus activation and profiles versus enforcement.

Acceptance: a released binary can export/install the skill without network
access; examples refer only to implemented commands; two users with different
hosts receive the same bundle; adding a host requires no regeneration; updates
and uninstall preserve custom files; an incompatible API/skill combination fails
clearly. Test installation into an empty root, interrupted updates, modified
managed files, path collisions, and unsafe links.

### AI-05: Durable headless creation, receipts, and cursor output

Dependencies: AI-01 and AI-03; updates AI-04's generated reference after landing.

Extend `internal/daemon/lifecycle.go`, `internal/storage`, `db/migrations`,
`internal/worker`, `internal/protocol`, and CLI remote/local adapters. Reuse worker
launch and recovery-reservation concepts. Add an explicitly supported durable
create operation, request lookup, finite cursor logs, and structured signal/kill
wrappers. Keep legacy interactive create semantics intact.

Acceptance: duplicate concurrent requests and retries after a dropped response
produce one session; a conflicting key fails; daemon restart returns the original
receipt; terminal/hibernated/interrupted sessions are not recreated by retry;
ambiguous launch outcomes are visible and not automatically replayed. Requests
cancel independently from accepted work. Quota/retention behavior is tested.

Add fault injection at reservation, persisted launch intent, worker startup,
metadata publication, receipt persistence, and response delivery. Exercise a
real detached worker and daemon restart, not only mocked maps. Verify cursor
continuity through daemon restart, wraparound, session exit, and recovery attempt
changes; report unavailable rather than invent offsets for legacy retained tails.

### AI-06: Opt-in scoped credentials and authorization

Dependencies: AI-01's contract plus the reviewed D23 amendment/security design in
section 5. This is a separate release track, not an implicit requirement for
AI-02 through AI-05.

Add a focused `internal/access` boundary and integrate server-derived principals
through `internal/transport`, daemon dispatch/relay, and `internal/sshd`. Keep
policy management local initially. Audit every legacy path and define how it is
denied or mapped in scoped mode before exposing an agent credential workflow.

Acceptance: authenticated read-only credentials can inspect permitted metadata
but cannot create, attach, input, signal, kill, resume, update, alter profile/grants,
or invoke a legacy control path to bypass restrictions. Output access is tested
separately. Wrong-target/replayed authentication, revocation on a live connection,
unknown operations, old peers, and policy races fail closed. A disconnected or
revoked controller does not kill existing workers.

Do not mark this milestone complete based on a CLI-only deny check. Security
review and an end-to-end bypass matrix are release requirements. Document the
same-OS-account and intentional serving boundaries explicitly.

### AI-07: Conformance, documentation, and staged rollout

Dependencies: AI-02 through AI-05 for core; include AI-06 evidence only when scoped
mode is being released.

Add reusable fake-host/clock fixtures, protocol goldens, real-process integration
coverage, and a focused `scripts/check-agent-interface.sh`. Update the user docs,
README links, operation reference, and implementation status only as functionality
actually lands. Keep permission claims conditional on the released track.

Acceptance: a fresh Mesh setup can install the skill, annotate a router, discover
a workstation, create work there with a saved key, lose the client connection,
and find the same work from another client. The router is not mutated by the
read flow. Record operator checks separately from automated results.

## 7. Required regression matrix

| Scenario | Required assertion |
| --- | --- |
| No hosts / uninitialized local installation | Valid empty/uninitialized result; no bootstrap side effects. |
| Host offline, timeout, or partial protocol support | Bounded response, explicit freshness/unsupported state, no implicit wake. |
| Old alias collides with a new command | Config still loads; `mesh --host ALIAS` targets it; no automatic rename. |
| Alias renamed / address reused / identity changed | Notes follow pinned identity, never a recycled name or IP. |
| Two profile editors / interrupted profile write | One CAS winner; conflict or previous valid profile survives. |
| Malicious text or credential-looking terminal output | Data stays data; no instruction execution, unsafe terminal rendering, or inclusion in the skill. |
| Inspection of an attached or hibernated session | No steal, resize, resume, input, or sleep-state change. |
| Lost create response / simultaneous retry | Same receipt and at most one accepted launch for the key. |
| Daemon death around process startup | Preserved worker is reconciled or uncertainty is explicit; no blind rerun. |
| Retry after job exit, hibernation, or host reboot | Original identity/outcome returned; no new job from the old key. |
| Log wraparound / old durable tail / wrong-session cursor | Gap or unsupported state explicit; no fabricated byte continuity. |
| CLI parse error / blocked stdin / update reminder | Finite machine-readable failure, no prompt and no polluted JSON. |
| New client, old daemon; new daemon, old worker | Capability fallback only for reads; no unsafe mutation downgrade. |
| Skill update, downgrade, custom edits, uninstall | Compatibility checked; customized files and private context preserved. |
| Scoped mode via legacy controls, SSH, or active connection | Current server policy enforced on every protected path, not just new verbs. |

Run the repository's required checks in implementation PRs:

```bash
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/verify.sh
./scripts/check-agent-interface.sh
```

The final command is a proposed new focused checker. Add profile/JSON/cursor fuzz
cases, generated-reference drift checks, and supported Linux/Darwin build checks.
Real power-loss and real-agent activation tests require operator evidence; do not
substitute a mocked test or a skill file's existence for those results.

## 8. Rollout, rollback, and boundaries

Ship passive profiles/reads and a read-only skill first. Release mutation-capable
skill guidance only after durable creation and its fault-injection suite land.
Scoped credentials ship separately after their architecture and bypass reviews.
Do not gate ordinary terminal startup on profile discovery or skill installation.

Use additive database migrations and independent profile/cache schemas. Test old
binaries against new state before documenting downgrade support. Unsupported
state must fail visibly, not be reset or deleted. Rolling back presentation or
skill files must not stop workers. Do not downgrade a scoped host to legacy mode
silently; returning to the old trust boundary requires an explicit local decision.

No new runtime dependencies are expected for core: JSON, embedding, the existing
protocol, and current persistence primitives should suffice. Any exception needs
the justification required by `CLAUDE.md` in its implementation task.

Explicit non-goals: scheduling or automatic host placement, cross-host process
migration, provisioning/configuration management, package installation probes,
project/agent-provider management, an MCP server, HTTP API, fleet wiki generator,
remote administration through free-form notes, reboot commands, and multi-user
sharing. Existing serving features remain exposure, not deployment (D22).

Completion means the relevant milestone tests and documentation exist in code;
merging this plan does not mark any milestone implemented. The plan's central
contract remains: **one generic skill, current host-specific context, persistent
sessions, and no stronger security claim than the actual enforcement boundary.**
