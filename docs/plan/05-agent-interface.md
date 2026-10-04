# Agent interface, host and fleet context, and bundled skill

Status: Approved, 2026-10-02.
Baseline: `main` at `7ba5f2b` (v0.1.122). This document adds no runtime behavior.

## Outcome

An agent can discover Mesh, read the bundled skill, choose a suitable host, read
the owner's operational context, run a command, retrieve its output and result,
and manage its own sessions without help from the owner and without driving a
picker or taking over a human's terminal. Humans keep the current terminal,
picker, colors, full-screen applications, nested detach keys, and session
recovery.

Mesh remains a session system. Machine context helps a caller choose and use a
host; it does not turn Mesh into a scheduler, deployment product, configuration
manager, or coding agent. One bundled skill teaches the workflow. Live Mesh
queries provide the user's machine-specific and fleet-wide information.

The split is the central decision:

- **The skill is release-owned and generic.** It is embedded in the binary,
  identical for every installation, and never contains per-machine content.
  It travels with Mesh through installation, adoption, and fleet updates, and an
  agent can read it without a source checkout or a network download.
- **Context is owner- or agent-authored and read live.** A host profile describes
  one host and is written on that host. A fleet profile describes the whole fleet,
  is written once, and is replicated to every member.

Example: a host's observed platform is Linux/arm64, while its owner-authored
profile says it is the home router and must not be rebooted or used for builds.
The fleet profile says how to set up a new machine. These are different kinds of
information. None of them is an authorization grant.

This plan has two delivery tracks:

- **Core:** host and fleet profiles, passive discovery, structured session
  operations, restart-safe creation receipts, pipe execution with durable results,
  and an installable generic skill.
- **Scoped access:** separately reviewed, opt-in authentication and authorization
  for agent credentials. Core must not advertise this protection before it exists.

All commands, packages, fields, limits, and tests described as additions below
are planned. Existing behavior is identified explicitly in the baseline.

## 1. Evidence and repository baseline

A Mac agent used `mesh pc -- command` to read source on the PC. The session
survived detachment, produced further output, reattached, and exited. The run
exposed these costs, all still present on the baseline:

- Results contained ANSI controls, terminal queries, and session notices. Calls
  that allocated a local terminal and calls that did not both received colored
  output from the remote PTY.
- Every short command created a session. The agent had to infer how to identify,
  retain, and eventually remove its own work.
- `mesh ls` and `mesh serve ls` print display tables only. `mesh update`,
  `mesh version`, `mesh app`, and the privileged wake helper already have
  `--json`, each with its own shape.
- Explicit attachment can take over another viewer. Safe inspection needs
  prominent guidance.
- No Mesh usage skill exists in the repository or the standard agent skill
  directories. `CLAUDE.md` describes development; the agent-recovery guide
  describes provider conversation recovery.

Terminal output has two producers: Mesh presentation and the child program. The
worker gives every child a PTY, which invites colors, pagers, and redraws.
Suppressing Mesh decoration fixes only the first producer. An ANSI stripper cannot
reconstruct terminal redraws or restore separate stdout and stderr; that needs a
pipe session (section 3.5).

| Existing surface | Reuse or constraint |
| --- | --- |
| [`internal/cli/config.go`](../../internal/cli/config.go) | `HostRecord` persists stable ID, pinned Mesh identity, and connection information. Destination-owned names and revisions come from authenticated declarations in the separate claim cache. `hosts.json` is versioned; obsolete viewer labels are discarded on an ordinary write. Do not create a second address book or put profiles into this file. |
| [`internal/cli/catalog.go`](../../internal/cli/catalog.go) | Concurrent host queries fall back to cached session rows and mark them `Stale`. Extend this pattern, not the meaning of a cached observation. |
| [`internal/cli/command.go`](../../internal/cli/command.go) | Human CLI routing, session inspection, and create/attach boundaries. Bare names can be hosts or sessions; declared machine names must be command-safe; exact IDs disambiguate collisions. |
| [`internal/protocol/control.go`](../../internal/protocol/control.go) | `host.info`, session list/create/inspect/logs, lifecycle controls, services, stable error codes, a session `Label`, and bounded log tails (`MaxLogTail`, 1 MiB). Extend the shared protocol rather than opening another control server. |
| [`internal/protocol/state.go`](../../internal/protocol/state.go) | `state.watch` streams host state and `host.metrics`. Version one of this plan uses finite reads; profile revisions can join the watch snapshot later. |
| [`internal/daemon/lifecycle.go`](../../internal/daemon/lifecycle.go) | Creation deduplicates request IDs by digest in an expiring in-memory map. That is not a durable receipt across daemon restarts. Log reads are bounded tails, not a cursor API. |
| [`internal/worker`](../../internal/worker) | One worker owns one PTY and one process. A pipe session kind extends worker launch and output retention; it does not add a second launcher. |
| [`internal/update`](../../internal/update) | `fleet.json` beside `hosts.json` lists fleet members by public identity. Daemons accept signed controls from their own identity and from administrators enrolled in `updates/administrators.json` (`mesh update trust`). The fleet profile reuses both. |
| [`cmd/mesh/main.go`](../../cmd/mesh/main.go) | `plainAgentCommand` routes selected commands around Fang presentation. New JSON paths need equivalent guarantees, including failures before command execution. |
| [`internal/cli/agent_setup.go`](../../internal/cli/agent_setup.go) | `mesh agent setup` manages native conversation-recovery hooks, and `mesh agent` runs native providers. Neither is repurposed for operational skill installation. |
| [`scripts/install.sh`](../../scripts/install.sh), `internal/bootstrap`, `internal/update*`, `.goreleaser.yaml` | Distribution. The installer requires the release archive to contain one file named `mesh`; embedding the skill preserves that contract. |
| [`CLAUDE.md`](../../CLAUDE.md), [overview](00-overview.md), [decisions](01-decisions.md) | Workers own PTYs, sessions belong to hosts, and the daemon protocol is the extension point. D23 delegates direct-listener admission to Tailscale; destination identity pinning is not client authentication. |

Preserve these invariants throughout implementation:

1. A disconnected client, expired API request, or restarted daemon does not kill a
   running session. A reboot is reported honestly; RAM is not resurrected.
2. Terminal traffic remains direct to the owning host. No central fleet database,
   coordinator, or new relay is introduced. The fleet profile is replicated to
   every host, each host keeps its own copy, and no host serves it for another.
3. Read operations never attach, steal, resize, inject input, wake a host, start a
   stopped daemon, fetch a served URL, or install software. Updating a local cache
   is allowed; changing the target is not.
4. Existing human CLI, SSH, recovery, hibernation, serving, and update behavior
   remains compatible unless a separate explicit decision changes it.
5. New functionality is generic: no hardcoded personal domains, machine names,
   agent provider, project layout, or assumed installed toolchain. Owner-specific
   content lives only in profiles.

## 2. Architecture and ownership

```text
Bundled Mesh skill, identical for every installation
                   |
                   v
Versioned CLI results and operation reference
                   |
                   v
Existing Mesh daemon protocol
       |                    |                       |
Host profile          Fleet profile replica    Existing sessions/workers
(written here)        (signed, replicated)     (PTY and pipe kinds)
       |                    |
Local client cache, keyed by pinned host identity
```

Keep three sections separate in every context response:

- `observed`: measured platform, reachability, and advertised runtime features.
- `declared`: owner- or agent-authored context, with `host` and `fleet`
  subsections that each carry their own revision and status.
- `access`: the actual admission/enforcement mode and, only when authenticated
  scoped access exists, the caller's effective permissions.

A label such as `role=router` is data, not an implicit denial rule. A note saying
Docker is installed is a declaration, not a successful live probe. Inspection
must not execute owner-supplied probe scripts or enumerate arbitrary credentials,
process environments, package inventories, or home-directory contents.

### 2.1 Host profile

Use JSON to match existing configuration conventions and avoid a new runtime
parser dependency. Editing through a temporary human-friendly document is a CLI
concern; it does not require another authoritative representation.

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

Store the authoritative `profile.json` beside `hosts.json`, using `ConfigPath()`
and its environment overrides rather than another home-directory convention. It
describes this installation's own stable identity, not its peers. Host profile
writes are **local only**:

```bash
mesh profile show --json
mesh profile set --file profile.json --expected-revision 0 --json
mesh profile edit
```

`edit` reads a revision, opens a temporary file, validates the result, and performs
the same compare-and-swap as `set`. It is human-only and never called by the skill.
An agent running on the host may use `set` with an expected revision. Reject
concurrent edits with `profile.conflict`; do not silently choose a winner. Use a
cross-process lock, a same-directory temporary file, file synchronization, atomic
replacement, and the repository's durability conventions. Create private
files/directories, reject unsafe symlink targets, and retain the previous valid
profile when validation or publication fails.

Limits: 32 KiB encoded profile, 1 KiB description, 16 KiB instructions, 32 labels
with 64-byte keys and 256-byte values, and 16 warnings of at most 512 bytes each.
Reject duplicate JSON keys, unknown fields, invalid UTF-8, invalid schema
versions, and disallowed control characters. Do not truncate accepted warnings or
instructions. Markdown in instructions remains text, never a command, include
directive, or URL that Mesh automatically fetches.

An absent profile, an invalid profile, an unsupported daemon, and an offline host
are distinct states. Invalid context must not make otherwise readable session
metadata disappear. It must be visible to the caller and prevent the skill from
assuming that it has read the owner's complete instructions.

Profiles are operational notes, not secret storage. In Tailnet mode they have the
same practical trust boundary as the host account and control listener. Local-only
editing does not make them tamper-proof against a caller that can already execute
arbitrary commands as that account.

### 2.2 Fleet profile

The fleet profile holds context every host should see: conventions that apply to
the whole fleet and runbooks such as setting up a new machine. It uses the host
profile's editable schema with a larger instructions limit. It is advisory text,
like a host profile. Mesh never executes it.

Example editable input:

```json
{
  "schemaVersion": 1,
  "description": "Personal fleet: workstation, laptop, Pi, router",
  "labels": { "workspace": "/work" },
  "warnings": ["Large downloads and build output go to /work, never the system disk."],
  "instructions": "## Set up a new machine\n1. Install the toolchain: Go, Bun, git, gh.\n2. Install the agent CLIs.\n3. Link ~/.claude/skills, ~/.codex/skills and ~/.config/agents/skills to ~/.agents/skills.\n4. Start the Fregat machine server.\n5. Check that /work is mounted and has projects/, worktrees/ and tmp/.\n\n## Conventions\n..."
}
```

The persisted envelope adds `fleet` (the saved fleet's name), `revision`,
`updatedAt`, `author` (the signing host's public identity), and `signature`, an
Ed25519 signature by `author` over the canonical encoding of the rest of the
envelope. Limits match the host profile except instructions, which allow 64 KiB,
and the encoded document, which allows 96 KiB.

**Who can write it.** A host accepts a fleet profile only when `author` is its own
identity or an administrator enrolled in its `updates/administrators.json`. Those
identities can already install a Mesh binary on the host, so trusting them for
advisory text adds no authority. Any other signer is rejected with
`fleet.signer_untrusted`, and the host keeps its current copy. A newly adopted host
accepts the fleet profile once the coordinator is enrolled there with
`mesh update trust`; the rejection names that fix.

**Where it lives.** Each host stores its replica at `fleet/profile.json` in the
Mesh state directory, because the daemon writes it on receipt. The host profile
stays in the config directory, because only the local user writes it.

**Who receives it.** Members of the saved fleet, `fleet.json` beside `hosts.json`,
the same membership `mesh update` uses. An address book does not establish fleet
membership. With no saved fleet, a write updates only the local replica and says so.

```bash
mesh profile show --fleet --json
mesh profile set --fleet --file fleet-profile.json --expected-revision 3 --json
mesh profile edit --fleet
mesh profile status --fleet --json
```

**Writing.** `set --fleet` compares `--expected-revision` with the local replica,
signs revision N+1 with the local host identity, stores it locally, and records
one pending delivery per fleet member in a durable outbox. It returns after the
local write with the delivery state of each member. The owner or an agent may
write it; the revision check is the only concurrency control.

**Replication.** The daemon delivers the newest signed document to each pending
member over a new negotiated control, `fleet.profile.put`. The target verifies the
signature and signer, then applies it only when its revision is higher than the
current replica. Replay of an older or identical document is a no-op, so the
signed document needs no nonce and any carrier is safe. A newer write replaces
older pending deliveries. Delivery retries with backoff while a member is offline,
never wakes a host, and gives up after 7 days with a `warn` and a `stale` state
in `status`; a later write or `mesh profile status --fleet --retry` queues it
again. `mesh add` delivers the current replica to the new host as part of
adoption. A member that lacks the control reports `protocol.unsupported` and stays
pending until it updates.

**Conflicts.** Two writers that both start from revision N each sign an N+1. A
member keeps the first N+1 it accepts and rejects the other with
`fleet.revision_conflict`, which the losing writer reports. `status` shows members
that hold different digests at one revision. The fix is a new write at N+2 with
the expected revision checked, which supersedes both everywhere. Mesh never merges
text.

**Reading.** The fleet profile appears in `declared.fleet` of every context
response, read from that host's own replica, with revision, digest, author, and
status (`absent`, `valid`, `invalid`, `untrusted`). `mesh hosts --json` returns the
local replica's fleet profile once at the top level and each host's replica
revision and digest, so a stale or divergent member is visible. A host profile is
more specific than the fleet profile for that host. When they disagree, the skill
reports the conflict instead of choosing.

Out of scope: more than one fleet per host, per-member overrides inside the fleet
profile, and file payloads. Syncing the owner's `~/.agents/skills` across hosts is
a possible later extension of the same signed replication; it is not part of this
plan.

### 2.3 Discovery, observations, and caching

Add `mesh hosts --json` for a compact selection view and `mesh inspect HOST --json`
for full host context. The compact view includes description, labels, complete
warnings from both profiles, profile status/revisions, and an indication that full
instructions must be read. It does not silently summarize away safety-relevant
notes.

Start with cheap, well-defined observations: platform/architecture of the running
daemon, host identity, Mesh build, advertised protocol features, and reachability
from this client. Do not infer GUI availability, tool installation, or task
suitability from the OS. More observations require named, bounded collectors in a
later change; no arbitrary executable probes in this work.

Each independently fetched section carries its source, client `receivedAt`, any
host `observedAt`, and freshness/status. Host wall clocks are not trusted for
cross-machine ordering or authorization. Use profile revisions for update order
and client receipt time for cache freshness.

Cache host context separately from `hosts.json`, following the catalog cache
boundary. Cache keys include stable host ID and pinned identity, not display name, IP, or
a raw filename supplied by a peer. Destination renames preserve context; an identity
change requires explicit re-adoption and cannot inherit the old profile silently.
Reject profile revision regression for the same identity and report it rather than
erasing a newer known warning.

`hosts` queries the bounded saved host set concurrently with a two-second default
budget. `inspect` has a five-second default budget. Both accept finite `--timeout`
and `--cached`; cached-only mode makes no network calls. Partial responses retain
successful sections and mark failures individually. Unreachable does not mean
asleep, and a last-known capability is not proof that it is currently available.

Treat the local host deliberately: D29 keeps it outside its own address book.
Include it without adding a fake `HostRecord`, creating an identity during a read,
or delaying local terminal-window startup. Missing local initialization is a
reported state, not permission to bootstrap it.

### 2.4 Access information must tell the truth

Before the scoped-access track lands, return an explicit Tailnet description:

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
The fleet profile's signer check protects the replica's integrity; it says
nothing about who may run commands.

## 3. CLI and protocol contract

Session operations require an explicit `--host` and can pin the resolved target
with `--expect-host-id`; the skill always supplies the pin after discovery. The
daemon validates the expected identity before any mutation.

| Surface | Behavior |
| --- | --- |
| `mesh hosts --json` | Local host plus saved peers, selection context, fleet profile, freshness, and per-host errors. |
| `mesh inspect HOST --json` | Full observed/declared/access context for exactly one host. |
| `mesh profile show\|set\|edit [--fleet]` | Host profile administration on this host, or fleet profile administration and replication with `--fleet`; `edit` is interactive, others support JSON. |
| `mesh profile status --fleet --json` | Per-member fleet profile revision, digest, and delivery state. |
| `mesh session list --host HOST --json` | Bounded, pageable session summaries with kind and label; no terminal output by default. |
| `mesh session inspect --host HOST ID --json` | Exact durable state plus available live inspection; preview requires an explicit flag. Never attaches. |
| `mesh session create --host HOST --kind pty\|pipe --request-key KEY --cwd PATH --json -- CMD...` | Create a persistent session and return a durable receipt without attaching. |
| `mesh session request --host HOST --request-key KEY --json` | Resolve a creation receipt or a known uncertain outcome after a lost response. |
| `mesh session wait --host HOST ID --timeout D --json` | Wait a finite time for completion. A timeout ends the wait, not the session. |
| `mesh session logs --host HOST ID --cursor CURSOR --max-bytes N [--stream stdout\|stderr] --json` | Finite page of output with an exact continuation cursor. |
| `mesh session signal\|kill\|rm --host HOST ID --json` | Explicit lifecycle mutation using existing semantics; no implicit creation, resume, or takeover. |
| `mesh serve ls --json` | Existing service listing in the structured envelope. Listing never fetches or starts a service. |
| `mesh skill show\|install\|update\|status\|uninstall` | Print or explicitly manage the bundled operational skill, separately from recovery hooks. |

`mesh ls` keeps its human table; `mesh session list` is the structured,
host-explicit form. Other existing `--json` outputs (`update`, `version`, `app`)
keep their shapes and exit codes until a separate change moves them to the
envelope; the operation registry lists them as legacy formats.

No new attach, terminal input-injection, bulk kill, environment-copying, arbitrary
remote file-write, or remote host-profile-write API is included in core. The fleet
profile is the only replicated context, and only signed by a trusted identity.
Existing human commands still exist. The skill does not fall back to interactive
commands or raw SSH to bypass an unavailable or denied operation.

### 3.1 Structured result rules

Define typed version-one result DTOs and a small operation registry, shared by
validation, documentation generation, and conformance tests. Do not build a
generic RPC framework or a second model of session state.

JSON output is one envelope containing `schemaVersion`, `status`, `requestId`,
`data`, and `errors`. `status` is `ok`, `partial`, or `error`; every error contains a
stable `code`, human `message`, affected target/operation, and `retryable` boolean.
Request IDs correlate calls. Creation request keys identify logical side effects;
these must not be conflated.

Use exit status 0 for a complete API result, 1 for operational failure, 2 for
invalid invocation, and 3 for useful partial results. An observed session's exit
code belongs in `data`; a completed job exiting 7 does not make a successful
inspection command exit 7. `data` distinguishes normal exit, signal termination,
interruption by host reboot, explicit cancellation, and Mesh transport failure.
Existing human CLI exit behavior remains unchanged.

JSON mode has no ANSI, update notices, spinners, picker, pager, or prompts, and
never waits on stdin for a decision; a missing decision is a bounded, actionable
error. Diagnostics go to stderr without duplicating the JSON result. Cover Cobra
parse errors and Fang handling, not just successful `RunE` paths. Respect `--` so a
remote program's flags cannot accidentally select local JSON behavior.

Initial stable errors include `host.unreachable`, `host.identity_mismatch`,
`protocol.unsupported`, `profile.invalid`, `profile.conflict`,
`fleet.signer_untrusted`, `fleet.revision_conflict`, `request.key_conflict`,
`request.in_progress`, `request.outcome_unknown`, `logs.cursor_expired`,
`result.expired`, `access.denied`, and `resource.limit`. Unsupported and unknown
capabilities must not collapse into false or permission denied.

Use the existing control framing and transports. Add explicitly negotiated
controls for full host context, fleet profile delivery, cursor logs, durable
creation, and pipe sessions. Do not attach a new key to legacy `session.create`
and assume an old daemon will honor it. An old peer must return unsupported
without launching anything. Freeze feature names and wire fixtures in the first
milestone; do not infer support only from release version strings. A preserved
worker can be older than its daemon, and an updated client does not update an
already-running worker.

### 3.2 Command-name compatibility

New names such as `hosts`, `inspect`, `profile`, `session`, and `skill` can already
be saved host aliases. Adding them to `reservedAliases` would make existing
`hosts.json` files fail validation.

Before registering new commands, add the explicit human escape form
`mesh --host ALIAS [-- CMD...]`. Separate validation of existing saved records
from validation of newly created/renamed aliases. Continue loading historical
aliases, reserve the new names only for new records, and print a clear migration
diagnostic without renaming anything. Explicit `--host` always reaches the saved
host, including one named `session` or `inspect`. Do not change ordinary
non-conflicting bare-host/session behavior.

### 3.3 Session creation and retry semantics

Creation is non-interactive. `--kind` is required, so a caller always knows
whether it gets a terminal or pipes:

- `pty` is a Mesh PTY session. Its command may rely on a terminal, and its
  stdout/stderr are combined by that PTY. Defaults are 80 columns, 24 rows, and
  `TERM=dumb`, with bounded optional geometry/terminal overrides.
- `pipe` runs without a terminal and keeps stdout and stderr separate
  (section 3.5).

Require an absolute target `cwd` and a non-empty argv. Execute the argv directly;
shell expansion requires an explicit shell command. The existing host execution
environment is used; this API does not copy the client's environment. Agent
sessions carry a label naming the creating client, so lists and cleanup can tell
them apart from human sessions.

A required opaque `--request-key` is scoped to target identity and, once available,
the authenticated principal. Tailnet-mode receipts state that weaker scope. The
server computes a digest of the complete normalized request, including kind and
all launch-affecting options. Reusing a key with different options fails before
launch; retrying it with the same options resolves the original result, even after
a daemon restart.

Extend the existing creation path instead of implementing another worker launcher:

1. Atomically reserve a request record and session ID in the owning host's durable
   storage before starting a process. Add an additive migration and storage
   methods; choose the next free migration number at implementation time.
2. Persist launch intent and carry the reserved identity/request fingerprint into
   worker startup. Serialize reservation ownership across processes. Reuse the
   recovery subsystem's durable-transaction concepts where appropriate, without
   changing what a recovery action means.
3. Start the detached worker, publish its metadata, and persist the receipt. The
   session handle is published before any output streams. A dropped response is
   not grounds for another launch.
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
maintenance. Automatic retries are not safe beyond the advertised retention window.
No background retry loop may switch to a new key.

Keep logical command lifetime separate from API deadlines. Cancelling the client,
timing out while awaiting a receipt, or a `session wait` timeout stops waiting, not
the accepted session. Cancelling the process is a separate explicit signal or kill.
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
stream, and output generation/attempt. Cursors are opaque strings, avoiding
JavaScript integer precision loss and accidental use against another session or
stream. Use 64 KiB pages by default and keep the existing 1 MiB upper bound.
Preserve byte-exact output in an encoded field; any human/plain-text rendering must
escape terminal controls and remain separate from cursor accounting.

Return `nextCursor`, completion state, and explicit retained-range information.
An expired cursor reports the gap and earliest available cursor; it must not
pretend missing bytes were delivered. Durable output from exited workers must have
explicit offset provenance or report cursor support unavailable. Do not synthesize
offsets for an older worker's unindexed tail. Retrieving output or a result never
reruns a command. Several readers may read the same session at once. No unbounded
`--follow` operation is required for version one.

### 3.5 Pipe sessions and durable results

A pipe session is a worker-owned session kind with the same host ownership,
detachment, and daemon-restart survival as a PTY session. It replaces screen
snapshots with stream replay.

- stdout and stderr are separate, byte-preserving streams, each sequenced from its
  own D1 offsets. Order is preserved within a stream; no combined observation order
  is promised.
- Mesh adds no terminal queries, colors, carriage-return conversion, banners, or
  notices to child output. Escape sequences the child emits deliberately are child
  data. A pipe cannot force every program to emit plain text, and the plan does not
  promise it.
- stdin is closed by default. `--stdin` forwards the caller's stdin until EOF;
  uncertain stdin writes after a disconnect are never replayed automatically.
- Signals go to the process group. Define Ctrl+C, descendant drain, and kill
  escalation on Linux and macOS, reusing D6's hangup-then-insist order.
- Output from a command that exits before any client reads it is kept. Completion
  is recorded after the final captured output.
- Retained output and queue memory are bounded per stream. A slow or absent reader
  never blocks the command; overflow is reported as a retained-range gap.
- Completion, exit details, and retained output persist for the receipt retention
  period, with documented storage limits and eviction. A read after eviction
  returns `result.expired`.

Pipe sessions appear in lists and inspection with `kind: pipe`. They cannot be
attached. Recovery reports an interrupted pipe session and never reruns it. The
picker shows them so a human can see and remove them. A daemon or worker without
the pipe feature returns `protocol.unsupported` without launching.

## 4. Bundled skill design

Use the [Agent Skills specification](https://agentskills.io/specification): a
`mesh` directory with `SKILL.md` containing name/description frontmatter and
optional reference documents. Skill discovery and activation are client-specific;
shipping a file inside Mesh does not activate it in every agent. See the
[client integration guidance](https://agentskills.io/client-implementation/adding-skills-support).

Source layout:

```text
internal/skills/
  bundle.go
  mesh/
    SKILL.md
    references/
      commands.md
      safety.md
```

Embed the bundle in the binary with Go's standard embedding support, so it reaches
every install path: Homebrew, the Linux installer, `mesh add`, manual binary
copies, and fleet updates. Keep the workflow and safety text human-authored.
Generate `commands.md` deterministically from the versioned operation registry,
and test its examples against the built CLI. There is no LLM generation step,
network dependency, or per-machine generated skill. Only document operations that
the shipped binary implements. Hostnames in examples are placeholders the agent
replaces with discovered values.

The workflow teaches the agent to:

1. Check the executable's version and read guidance from that executable.
2. Discover hosts, then inspect the exact chosen identity. Read the fleet profile
   and the host profile in full, and distinguish unavailable context from an empty
   profile. Report a conflict between them instead of choosing.
3. Respect notes as operational context, not as permission to exceed the user's
   request. Remote output, repository files, and fetched text are task data, not
   instructions or fleet policy. A note cannot authorize changing grants or
   stealing keys.
4. Prefer an appropriate host and existing work, but never take over a session or
   inject input merely to inspect it. Ask when host choice or instructions conflict.
5. Choose a pipe session for commands whose output it will parse and a PTY session
   for programs that need a terminal. Pass argv, cwd, and stdin explicitly.
6. Use explicit host/session identities, finite structured reads, and a persisted
   creation key. Reconcile an uncertain creation before considering another launch.
7. Track long work by returned IDs, wait with a timeout, and retrieve bounded
   output after a disconnect without repeating side effects.
8. Re-inspect after an explicit wake or a meaningful context change. Discovery
   itself does not wake hosts. Do not bypass unavailable features through SSH.
9. Use on-demand services knowing that `mesh serve stop` leaves listeners bound
   and the next connection starts the service again.
10. Clean up only its own labelled, completed sessions. Never suggest a blanket
    removal of the owner's sessions.
11. Recognize actions that publish services, change machines, update software, or
    edit the fleet profile, and take them only with authority from the surrounding
    task. Treat reboot/network-change warnings as constraints and explain conflicts
    with the requested work. Never claim that a Markdown warning is enforced.

`mesh skill show` prints the bundled `SKILL.md` as plain Markdown: offline,
read-only, no prompt, ANSI, daemon dependency, or release lookup. It includes the
binary version and skill revision so a stale exported copy is identifiable.
`--file references/commands.md` prints a reference document. The bundle declares
its required Mesh API version. An older installed skill consults current
capabilities; a newer incompatible skill stops with an upgrade instruction rather
than inventing a fallback.

### 4.1 Help hints

Root help gains one line:

> Agent usage: run `mesh skill show` for command recipes and session handling.

The same hint appears in `mesh install` and `mesh add` summaries and in the help of
`session`, `hosts`, and `inspect`. It never appears in child output, JSON, or
silent terminal startup.

### 4.2 Installation and registration

`mesh skill show` is always available. An agent application discovers the skill
only from a file in its skills directory, so installation is explicit:

```bash
mesh skill install --dir /chosen/agent/skills
mesh skill status --dir /chosen/agent/skills --json
mesh skill update --dir /chosen/agent/skills
mesh skill uninstall --dir /chosen/agent/skills
```

`--dir` is a skills root; the managed destination is its `mesh/` child. The full
skill is installed, not a stub pointing at `mesh skill show`, so an agent knows how
to begin before running a command. Validate containment and symlinks. Use an
install manifest containing managed paths, version, and content digests. Stage
complete updates before publishing them. Never delete or overwrite user-modified
files silently: report a conflict and keep both the current installation and the
proposed replacement. Uninstall removes only unchanged managed files and reports
preserved customizations. A failed install or refresh leaves a working Mesh binary,
reports the problem, and can be repeated.

Opting in is recorded. Each install adds its root to a local registry,
`skills.json` beside `hosts.json`, and uninstall removes it. After an update
activates a new binary, the new daemon refreshes every registered root once and
reports the result in `mesh skill status` and the update result. Hosts that never
opted in get no skill files.

Registration choices are explicit at every entry point:

- Interactive `mesh install` and `mesh add` offer registration with a directory
  prompt. Unattended runs register only with an explicit `--skill-dir`.
- A remote `mesh add --skill-dir DIR` registers on the target using the target
  binary's own bundle. Local registration never changes a remote host's agent
  configuration.
- Global Mesh installation never writes a skill into a repository.

Provider targets (`--target claude|codex|…`) are a later adapter. Before adding one,
verify that provider's current user- and project-level skill paths, link support,
and configurable data homes, and test it separately. Do not edit provider hook
settings, grant tool permissions, or install into every agent discovered on disk.
No profiles, hostnames, keys, session output, or access grants are copied into the
skill or emitted by its installation commands.

## 5. Scoped-access follow-up and its release gate

D23 says the direct listener has no second client authentication/authorization
layer. A read-only agent credential therefore requires a separate
architecture/security review, not an `allow` field in a profile. Record an explicit
amendment to D23 before implementing opt-in scoped mode; preserve D17's transport
choice and D18's single host identity.

The design is a host-owned grant store and a server-verified caller principal,
distinct from the destination host identity. Reuse existing identity and
signed-control patterns, but review the authentication transcript and replay
protection before implementation. Specify challenge binding to target identity,
connection, protocol version, expiry, and a single-use nonce. Client-supplied
role/principal strings are never evidence of identity. Do not add a new transport
or implement new cryptographic primitives.

Scoped mode fails closed on unknown/revoked credentials and on Tailnet-mode
unauthenticated control requests. It cannot coexist with an unrestricted route to
the same protected control operations. Tailnet-only mode remains an explicit
compatibility choice, never a silent fallback for a scoped credential.

Start with explicit operation permissions, not an inference from machine labels:
metadata/context read, session detail read, output read, session create, session
mutation, fleet profile write, and administrative operations. Logs/previews need
their own permission because read-only output can still disclose secrets. Protect
profile and grant edits separately; an execution agent does not get them by
default.

Authorization must cover the actual dispatch/relay paths, not only new CLI
commands. Audit direct WebSocket controls, SSH commands and channels, session
attach/input/resize, recovery and resume, update/bootstrap operations, fleet
profile delivery, serving controls, and other paths capable of starting work.
Check current grants at the moment of each effect. Revoke active control access
without killing the detached session. Missing policy support in an older
daemon/worker must never become an allow decision.

Local same-user worker sockets and arbitrary shell access are part of the OS trust
boundary, not a sandbox. An agent with the owner's account, administrator
credentials, or arbitrary execution as that account can bypass text guidance and
may bypass Mesh-only restrictions. Document this plainly. Public/private serving
routes with intentional on-demand behavior also need an explicit threat-model
boundary; inspecting a service is not permission to fetch its URL and start it.

No reboot-management feature is added here. The router example is satisfied first
by owner context; strong isolation requires appropriate credentials and OS/network
permissions. A `--yes` flag or submitted profile revision is not independent human
approval.

This track is not required to ship advisory profiles and a skill. It is required
before advertising per-agent enforced permissions, enabling remote host-profile or
grant administration, or describing an agent key as securely read-only.

## 6. Implementation sequence

The identifiers below are local to this plan. They do not reserve or renumber the
repository's `Txx` tasks. Each milestone is a separately reviewable implementation
PR with tests, ends with working commands and matching skill text, and can be split
into task briefs.

### AI-01: Freeze contracts and preserve CLI compatibility

Dependencies: none.

Touch `internal/protocol`, `internal/cli/command.go`, `internal/cli/config.go`,
`cmd/mesh/main.go`, and a small new `internal/agentapi` package for result DTOs and
operation metadata. Keep session state types shared with the existing protocol.

Deliver version-one JSON schemas/fixtures, error and exit-code tables, finite
limits, protocol feature names (including fleet delivery and pipe sessions), the
timeout/cancellation/stdin/stream-ordering/retention contracts of sections 3.3–3.5,
and the explicit `--host` escape. Inventory the existing `--json` outputs in the
registry. Implement the split between loading historical aliases and validating
new aliases before registering the new commands. Add tests for JSON
parsing/validation failures, stdout cleanliness, and an old saved host named after
each new command.

Acceptance: existing non-conflicting CLI behavior is unchanged; old address books
still load; explicit host selection is unambiguous; unsupported new controls have
no side effects. Schema examples and operation-reference metadata are tested.

### AI-02: Host profiles and bounded context cache

Dependencies: AI-01.

Add `internal/hostprofile` for validation, revisions, and atomic local storage;
use `internal/paths` and existing config resolution. Extend the client cache behind
`internal/cli/catalog.go` or an adjacent adapter rather than changing `HostRecord`.
Add the local profile commands and the read-only host-context protocol handler.

Acceptance: exact round trip of multiline instructions; absent/invalid/offline
states distinguishable; compare-and-swap survives concurrent writers; crash or
disk-full during publication preserves the previous profile; aliases do not own
cache identity; inspection never runs profile content or exposes unrelated files.
Test config overrides, symlink rejection, oversized/malformed JSON, profile
clearing, identity replacement, and revision regression.

### AI-03: Fleet profile and replication

Dependencies: AI-02.

Extend `internal/hostprofile` with the signed fleet envelope. Add the replica store
under the state directory, the durable outbox and delivery loop in the daemon, the
`fleet.profile.put` control, `--fleet` on the profile commands, delivery during
`mesh add`, and `declared.fleet` in host context. Reuse `internal/update` for fleet
membership, host keys, and the administrator policy; do not add a second trust list.

Acceptance: a write on the coordinator reaches every reachable member, and an
offline member receives it when it returns without being woken. A member rejects an
unknown signer, a tampered document, and an older or replayed revision, and keeps
its replica. Two concurrent writers produce one `fleet.revision_conflict` and a
visible divergence that a later write resolves. Give-up after 7 days logs once and
shows `stale`. An old member reports unsupported and stays pending. No fleet file
means a local-only write that says so.

### AI-04: Passive agent discovery and session reads

Dependencies: AI-01 and AI-02; shows fleet context once AI-03 lands.

Wire `hosts`, `inspect`, session list, session inspect, and `serve ls --json`
through `internal/cli/catalog.go`, the CLI inspection boundary,
`internal/daemon/lifecycle.go`, `internal/protocol`, and worker capability checks.
Add pagination and explicit per-section freshness. Cursor logs may land with
AI-06; do not advertise them before both live and durable-output paths conform.

Acceptance: local results do not require network discovery; one unreachable peer
produces bounded partial output; an unavailable host is distinguishable from an
empty catalog; all failures are structured; a busy terminal keeps its attacher and
geometry; hibernated sessions remain stopped; no read wakes hosts, launches
daemons, fetches services, or starts on-demand commands. Test old workers under new
daemons as well as fully old peers.

### AI-05: Bundle, help hints, and explicit skill installation

Dependencies: AI-04 for the initial read-only skill; expand only as later commands
land.

Add the embedded `internal/skills` bundle, deterministic command-reference
generation, install manifest handling, the `skills.json` registry, `mesh skill`
commands, help hints, registration prompts and `--skill-dir` in `mesh install` and
`mesh add`, and the post-update refresh. Do not modify `mesh agent setup` or
provider recovery hooks. Add user documentation explaining skill installation
versus activation and profiles versus enforcement.

Acceptance: a released binary prints and installs the skill without network
access on each release platform; printed content matches the canonical source;
examples refer only to implemented commands; two users with different hosts
receive the same bundle; adding a host requires no regeneration; updates and
uninstall preserve custom files; an incompatible API/skill combination fails
clearly. Test an empty root, interrupted updates, modified managed files, path
collisions, unsafe links, denied writes, no agent installed, multiple roots, and a
refresh failure that leaves Mesh usable. Validate Homebrew, Linux installer,
manual copy, remote adoption, and fleet update.

### AI-06: Durable creation, receipts, and cursor output

Dependencies: AI-01 and AI-04; updates AI-05's generated reference after landing.

Extend `internal/daemon/lifecycle.go`, `internal/storage`, `db/migrations`,
`internal/worker`, `internal/protocol`, and CLI remote/local adapters. Reuse worker
launch and recovery-transaction concepts. Add durable `--kind pty` create, request
lookup, `session wait`, finite cursor logs, and structured signal/kill/rm
wrappers. Keep interactive create semantics intact.

Acceptance: duplicate concurrent requests and retries after a dropped response
produce one session; a conflicting key fails; daemon restart returns the original
receipt; terminal/hibernated/interrupted sessions are not recreated by retry;
ambiguous launch outcomes are visible and not replayed. Requests cancel
independently from accepted work. Quota/retention behavior is tested.

Add fault injection at reservation, persisted launch intent, worker startup,
metadata publication, receipt persistence, and response delivery. Exercise a real
detached worker and daemon restart, not only mocked maps. Verify cursor continuity
through daemon restart, wraparound, session exit, and recovery attempt changes;
report unavailable rather than invent offsets for legacy retained tails.

### AI-07: Pipe sessions and durable results

Dependencies: AI-06.

Add the pipe kind to `internal/worker` launch, per-stream output retention, and
durable completion; extend `internal/protocol`, `internal/transport`, and
`internal/daemon/relay.go` for per-stream replay; update list, inspect, recovery,
and the picker for the new kind. Update capability negotiation and release
compatibility checks in the same change. Update the skill's execution recipes.

Acceptance: exact stdout and stderr bytes for Unicode, binary payloads, partial
lines, large output, stdin EOF, and exit codes, with the client's terminal both
allocated and absent. A fast exit keeps its output. A disconnect and a daemon
restart during execution leave the same process running, and its output and result
are retrieved without repeating side effects. Wait timeout, explicit cancellation,
signal termination, interruption after a simulated host restart, and output
expiration each produce a distinct result. A slow reader does not block the child.

### AI-08: Opt-in scoped credentials and authorization

Dependencies: AI-01's contract plus the reviewed D23 amendment and security design
in section 5. This is a separate release track, not a requirement for AI-02 through
AI-07.

Add a focused `internal/access` boundary and integrate server-derived principals
through `internal/transport`, daemon dispatch/relay, and `internal/sshd`. Keep
policy management local initially. Audit every Tailnet-mode path and define how it
is denied or mapped in scoped mode before exposing an agent credential workflow.

Acceptance: authenticated read-only credentials can inspect permitted metadata but
cannot create, attach, input, signal, kill, resume, update, alter profiles/grants,
deliver a fleet profile, or invoke a legacy control path to bypass restrictions.
Output access is tested separately. Wrong-target/replayed authentication,
revocation on a live connection, unknown operations, old peers, and policy races
fail closed. A disconnected or revoked controller does not kill existing workers.

Do not mark this milestone complete on a CLI-only deny check. Security review and
an end-to-end bypass matrix are release requirements. Document the same-OS-account
and intentional serving boundaries explicitly.

### AI-09: Conformance, documentation, and staged rollout

Dependencies: AI-02 through AI-07 for core; include AI-08 evidence only when scoped
mode is being released.

Add reusable fake-host/clock fixtures, protocol goldens, real-process integration
coverage, and a focused `scripts/check-agent-interface.sh`. Update the user docs,
README links, operation reference, and [implementation status](02-status.md) as
functionality lands. Keep permission claims conditional on the released track.

Acceptance: a fresh agent with only Mesh installed and help access discovers the
skill, installs it, reads the fleet runbook, annotates a router, discovers a
workstation, runs a short pipe command there, starts a long job with a saved key,
loses the client connection, retrieves the same job's output from another client,
and cleans up only its own work without taking over an existing human session. The
router is not mutated by the read flow. Record operator checks separately from
automated results.

## 7. Required regression matrix

Use isolated state directories and test processes. Preserve the owner's sessions.

| Scenario | Required assertion |
| --- | --- |
| No hosts / uninitialized local installation | Valid empty/uninitialized result; no bootstrap side effects. |
| Host offline, timeout, or partial protocol support | Bounded response, explicit freshness/unsupported state, no implicit wake. |
| Retained name collides with another owner | Bare-name targeting refuses ambiguity and prints exact IDs. |
| Destination renamed / address reused / identity changed | Notes follow pinned identity, never a recycled name or IP. |
| Two profile editors / interrupted profile write | One CAS winner; conflict or previous valid profile survives. |
| Fleet profile from an untrusted signer, tampered, or replayed | Rejected; replica unchanged; status names the cause. |
| Two fleet writers from one revision / member offline for days | Conflict reported and visible; later write converges; offline member catches up or shows `stale`, never woken. |
| Malicious text or credential-looking terminal output | Data stays data; no instruction execution, unsafe terminal rendering, or inclusion in the skill. |
| Inspection of an attached or hibernated session | No steal, resize, resume, input, or sleep-state change. |
| Lost create response / simultaneous retry | Same receipt and at most one accepted launch for the key. |
| Daemon death around process startup | Preserved worker is reconciled or uncertainty is explicit; no blind rerun. |
| Retry after job exit, hibernation, or host reboot | Original identity/outcome returned; no new job from the old key. |
| Pipe output bytes, stdin EOF, fast exit, slow reader | Exact per-stream bytes; output kept; child never blocked by the reader. |
| Wait timeout / cancel / signal / reboot interruption | Distinct structured results; a timeout leaves the session running. |
| Log wraparound / old durable tail / wrong-session or wrong-stream cursor | Gap or unsupported state explicit; no fabricated byte continuity. |
| CLI parse error / blocked stdin / update reminder | Finite machine-readable failure, no prompt and no polluted JSON. |
| New client, old daemon; new daemon, old worker | Capability fallback only for reads; no unsafe mutation downgrade. |
| Human picker, colors, full-screen programs, resize, nested keys, detached-only window claims | Unchanged. |
| Skill update, downgrade, custom edits, uninstall, refresh after fleet update | Compatibility checked; customized files and private context preserved; Mesh usable after a refresh failure. |
| Scoped mode via legacy controls, SSH, or active connection | Current server policy enforced on every protected path, not just new verbs. |

Run the repository's required checks in implementation PRs:

```bash
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/verify.sh
./scripts/check-agent-interface.sh
```

The final command is a new focused checker. Add profile/JSON/cursor fuzz cases,
generated-reference drift checks, and supported Linux/Darwin build checks. Real
power-loss and real-agent activation tests require operator evidence; do not
substitute a mocked test or a skill file's existence for those results. Record the
exact releases with the evidence.

## 8. Rollout, rollback, and boundaries

Ship passive profiles, the fleet profile, reads, and a read-only skill first.
Release mutation-capable skill guidance only after durable creation and its
fault-injection suite land, and pipe recipes only after AI-07. Scoped credentials
ship separately after their architecture and bypass reviews. Do not gate ordinary
terminal startup on profile discovery, fleet delivery, or skill installation.

Use additive database migrations and independent profile, fleet replica, and cache
schemas. Test old binaries against new state before documenting downgrade support.
Unsupported state fails visibly; it is not reset or deleted. Rolling back
presentation or skill files must not stop workers. Do not downgrade a scoped host
to Tailnet mode silently; returning to the old trust boundary requires an explicit
local decision.

No new runtime dependencies are expected for core: JSON, embedding, Ed25519 from
the standard library, the existing protocol, and current persistence primitives
suffice. Any exception needs the justification required by `CLAUDE.md` in its
implementation task.

Explicit non-goals: scheduling or automatic host placement, cross-host process
migration, provisioning/configuration management (a runbook in the fleet profile is
text an agent reads, not something Mesh runs), package installation probes,
project/agent-provider management, an MCP server, HTTP API, fleet wiki generator,
remote host-profile administration, reboot commands, and multi-user sharing.
Existing serving features remain exposure, not deployment (D22).

Completion means the relevant milestone tests and documentation exist in code;
approving this plan does not mark any milestone implemented. The plan's central
contract remains: **one generic skill, current host and fleet context, persistent
sessions, and no stronger security claim than the actual enforcement boundary.**
