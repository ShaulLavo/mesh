# Mesh updates itself everywhere

Status: Approved (owner, 2026-11-10). High priority. Design written 2026-11-11
against main `514700f4` (v0.1.306). An independent review comes before
implementation.

This is in-repository plan 17. It replaces the binary rollout steps of
[14: keep the fleet current](14-fleet-auto-updates.md) (steps 1, 2, 3 and 6)
and builds on [T26](../tasks/T26-mesh-updates.md). Plan 14's configuration
delivery and drift presentation stay. The "audit Plan 017" that plan 14 cites is
a different document: the audit wave that built the consent-bound coordinator
policy.

## Goal

Nobody updates Mesh by hand. When a release is published, every machine in the
fleet (PC, Mac, Pi, VPS and any machine added later) moves to it on its own.

On 2026-11-10 the PC and the Mac ran v0.1.249 while the latest release was
v0.1.306. The Pi and the VPS ran v0.1.255. `mesh update` to the latest release
refused, because v0.1.306 lists no tested transition from those builds and
reports a state read range of 15 to 16 against their state version 13. Catching
up meant finding and installing intermediate releases by hand. The Mac went
from v0.1.249 to v0.1.257 that way.

### Done means

1. Publishing a release is enough. Each machine notices it and updates with no
   command, prompt or approval.
2. A machine that was off or offline for weeks catches up on its own when it
   returns, however many releases it missed. Mesh plans and runs the chain.
3. Running sessions survive every update, including the sessions and services
   the PC hosts. A failed update rolls back on its own, is reported, and leaves
   the machine with a working Mesh.
4. Release CI refuses to publish a release that leaves any supported older
   release without a tested upgrade path to it.
5. The managing chat's hourly check sees every machine's Mesh version and is
   told when one has fallen behind or an update failed.
6. CI runs the real thing: an old installed release jumps several releases to
   the candidate with a live session that must survive.

### Constraints

- Update security stays at least as strong as it is now: the fixed HTTPS
  release origin, verified artifact hashes, exact tested transitions, and
  signed update controls from enrolled administrators.
- New machines enter the same automatic policy.
- No real fleet daemon changes before the rollout below. The PC hosts the model
  proxy and the managing chat, so it goes last, after managing is told.
- The owner allows compatibility code for the supported-older-release
  requirement. No new dependency is needed.

## What exists

The single-host transaction is solid and stays.

- **Release identity.** `internal/release/release.go` defines `Manifest`,
  `Artifact`, `Compatibility` and `Transition`. `Manifest.Allows`
  (`release.go:132`) admits an installed build only when the manifest lists a
  transition from that build's exact executable digest and the build's state
  and worker versions fall inside the manifest's ranges. A version label never
  authorizes anything.
- **Release origin.** `release.Client` (`internal/release/client.go`) reads
  `mesh-release.json` and archives only from
  `https://github.com/ShaulLavo/mesh/releases` over HTTPS, bounds every reply,
  and checks archive and executable SHA-256. Release artifacts carry no
  cryptographic signature. The signatures in Mesh updates are on update
  controls (`update.Message.Sign`, `internal/update/auth.go:47`), accepted from
  the host's own identity or an enrolled administrator
  (`Authority.IsAdministrator`, `auth.go:297`).
- **Installation.** `updateinstall.Engine` (`internal/updateinstall/engine.go`,
  `transaction.go`) stages a verified candidate, takes an activation grant, and
  hands activation to a separate helper service. The helper stops the daemon,
  switches the executable, starts the candidate, and checks the executing
  digest, the database version, and every retained worker's PID and shell PID
  (`verifyHealth`, `transaction.go:293`). A failed check restores the previous
  executable (`restore`, `transaction.go:223`). The journal survives reboot.
- **Publication.** `.github/workflows/release.yml` reserves a version, builds,
  proves a forward upgrade on native linux/amd64, linux/arm64 and darwin/arm64
  runners (`scripts/prove-release-transition.sh`), assembles compatibility from
  the receipts (`scripts/assemble-release-compatibility.py`), and publishes.
- **Release checks.** The daemon refreshes the latest manifest every six hours
  plus jitter (`internal/updatenotice/notice.go:18`, started at
  `internal/cli/command.go:1784`) and saves it as `CheckedRelease`.
- **Coordinator policy.** v0.1.295 added a consent-bound automatic policy
  (`internal/update/automatic*.go`, `mesh update automatic`). One coordinator
  pins one release for a saved fleet and pushes it to recipients that each
  authorized that exact fleet and epoch.

## What is broken

All findings were read from published manifests and from the PC on 2026-11-11.
The scripts and raw output are in
`/work/reports/mesh-auto-update/design-evidence/`.

1. **Nothing is stranded, and nothing walks the path.** Every release with a
   manifest (v0.1.41 onward, 250 releases) has a tested chain to v0.1.306 on
   all three platforms. From v0.1.249 it is seven hops: 249, 257, 266, 274,
   284, 293, 304, 306. From v0.1.255 it is six: 255, 264, 272, 281, 291, 302,
   306. From v0.1.257 it is six: 257, 266, 274, 284, 293, 304, 306.
2. **Each release proves eight predecessors.**
   `scripts/prepare-release-baselines.sh:59` requires only the previous release
   and adds the next seven when their archive downloads. 50 releases were
   published between v0.1.249 (October 17) and v0.1.306 (November 8), so a
   build leaves direct reach in about three and a half days.
3. **The optional sources drop silently.** v0.1.272 has no linux/amd64
   transition from v0.1.269. A failed download is a notice
   (`prepare-release-baselines.sh:47`), and publication continues.
4. **The state range is a test record.** `assemble-release-compatibility.py:58`
   sets `stateReadMin` to the lowest state version among the tested sources.
   Every executable embeds all sixteen migrations (`db/migrations`,
   `internal/storage/store.go:407`) and applies whatever is pending. "State
   version 13 is outside 15..16" means nobody tested it.
5. **The search stops at one intermediate.** `release.Client.Bridge`
   (`internal/release/bridge.go:44`) looks for a single stepping stone among
   the 32 newest releases within ten seconds and only suggests a command.
   v0.1.249 needs six stepping stones.
6. **The coordinator policy cannot meet the goal.** It fails a target whose
   build the pinned release does not allow (`prepare`,
   `internal/update/coordinator.go:319`) and never looks for a chain. None of
   the fleet's builds contains it, and the fleet was never enrolled. A member
   that stays offline keeps the run
   unsettled, and `AcceptCheckedRelease` (`automatic_store.go:307`) starts no
   newer release until the current run settles, so one sleeping laptop freezes
   every other machine. `consentFor` (`automatic.go:172`) binds each recipient
   to the exact membership digest and epoch, so adding a machine voids every
   consent. A failure is a sticky stop that a person must retry or acknowledge.
7. **The helper lags and nothing notices.** The PC's daemon is v0.1.249. Its
   helper receipt and `current` link name the v0.1.196 executable.
   `helperUpgradeAllowed` (`internal/updateinstall/helper.go:336`) refuses
   promotion when the running helper's own state version is outside the
   committed manifest's read range, so a helper outside that range can never
   be promoted. How the PC's helper first fell behind is not established; unit
   5 diagnoses it. The PC's helper unit is enabled and has been inactive since
   the boot on 2026-11-11. The automatic path checks only the receipt file
   (`RequireAutomaticHelper`, `engine.go:390`).
8. **Rollback after a migration is unproven.** Receipts certify that the
   candidate opened retained state. `restore` restarts the previous executable
   against a database the candidate already migrated, and no test runs that.
9. **Publication never exercises the installed path.**
   `prove-release-transition.sh` starts daemons by hand. The helper, the
   journal and the service manager are covered by Go tests with fake services
   and by an operator-run VM script (`scripts/check-updates-vm.md`).
10. **Upgrade breakage shows up after merge.** Transition proofs run only in the
    publisher. A pull request that breaks an upgrade passes CI.
11. **Installed readers are strict.** Every installed build decodes the
    manifest with unknown fields disallowed and requires exactly three
    artifacts (`client.go:253`, `release.go:205`). The helper decodes the
    journal the same way (`internal/updateinstall/storage.go:161`). Adding a
    field to either file locks every older build out of the release that
    carries it.

Outside the graph: v0.1.1 through v0.1.38 predate manifests, fourteen reserved
drafts were never published, and four tags have no release. No fleet machine
runs any of them.

## Principal decisions

1. **Each host updates itself.** A host updater inside the daemon plans and
   installs releases for its own machine with its own authority. It needs the
   internet and nothing else: no coordinator, no saved fleet, no enrollment.
   This fits invariant 6, where each host is authoritative for its own state.
2. **Direct reach is a release gate.** Each release proves direct transitions
   from every release since the newest anchor and from every supported anchor.
   Any supported release reaches the candidate in at most two hops. Publication
   fails when any published release has no chain.
3. **Anchors are the retained stepping stones.** Anchor releases are never
   deleted, and each proves transitions from the anchors before it. A machine
   that was off for a year still has a chain.
4. **Rollback is proven before publication.** The transition proof gains a leg
   where the previous executable reopens candidate-written state. The host
   updater uses only transitions with that proof.
5. **The manifest and journal formats are frozen.** New metadata ships as a
   separate release asset, the upgrade map. New updater state lives in new
   files.
6. **Fleet order comes from gates.** A host may wait until named peers have run
   a release for a soak period. The Pi and the VPS go first, then the Mac when
   it is awake, then the PC.
7. **One mechanism.** The consent-bound coordinator policy is removed after the
   fleet runs on the host updater. Manual `mesh update` operations stay.
8. **The existing fleet needs one approved hop per machine.** Installed code
   cannot learn new behavior from a published release. The activation release
   proves direct transitions from the three installed executables, so that one
   hop needs no stepping stone.

## Design

### Host updater

New code in `internal/update`: `host_updater.go`, `host_policy.go` and
`host_state.go`. `newUpdateController` (`internal/daemon/update.go:37`) builds
it beside the coordinator, and `internal/daemon/app.go:584` runs it once the
listeners are ready.

```go
// updates/host-policy.json, private like automatic.json (readAuthority).
type HostPolicy struct {
	Schema   int      `json:"schema"`
	Revision uint64   `json:"revision"`
	Mode     HostMode `json:"mode"`            // automatic | manual
	After    []string `json:"after,omitempty"` // host identities that run a release first
	Soak     Duration `json:"soak,omitempty"`
	GateWait Duration `json:"gateWait,omitempty"`
}

// updates/host-update.json
type HostUpdate struct {
	Schema      int               `json:"schema"`
	Phase       HostPhase         `json:"phase"` // idle | waiting | installing | blocked | attention
	Target      *PlannedRelease   `json:"target,omitempty"`
	Hops        []PlannedRelease  `json:"hops,omitempty"`
	Hop         int               `json:"hop,omitempty"`
	Request     string            `json:"request,omitempty"` // updateinstall.Request.ID in flight
	Problem     *HostProblem      `json:"problem,omitempty"`
	Quarantined []PlannedRelease  `json:"quarantined,omitempty"` // newest 8
	History     []CommittedUpdate `json:"history,omitempty"`     // newest 8
	NextAttempt time.Time         `json:"nextAttempt,omitempty"`
}

type PlannedRelease struct {
	Version  string `json:"version"`
	Manifest string `json:"manifest"` // manifest digest
	Binary   string `json:"binary"`   // executable digest for this platform
}
```

`Request` is set only while `Phase` is `installing`, and `Hop` always indexes
`Hops`. The constructor enforces both.

The updater is one idempotent step. It runs every 30 seconds on local reads,
and touches the network only when a check is due.

1. Read the policy. Manual mode is idle.
2. Read the installation journal (`updateinstall.Read`). An unfinished journal
   with another request ID means a manual or fleet operation owns the
   installation, so wait. A journal with our request ID is the hop in flight:
   committed advances the plan and appends to `History`; a rollback or failure
   goes to failure handling below.
3. Choose the target. With no gates it is the checked latest release. With
   gates it is the newest release every reachable gate has verified.
4. Stop when the executing digest equals the target's artifact or the
   executing version is newer. Exclude quarantined transitions while planning.
5. Check the installation: a Mesh-managed executable
   (`ValidateInstallationPath`), service preflight, required mount, free space,
   and a current, running helper.
6. Plan the chain and check every live worker against every hop.
7. Start the next hop: `Engine.Stage`, then `Engine.Grant`. The helper
   activates. The daemon exits during activation, and the next daemon resumes
   at step 2.

Before every hop the updater plans again from the executing build. The saved
hop list is a record for status, never an instruction.

**Authority.** The updater calls the engine in-process, as `mesh update --local`
does through the daemon today (`updateController.install`,
`internal/daemon/update.go:116`). A host's own identity is always an update
administrator on itself. The request carries no coordinator `Automatic`
reference. A separate local ownership record binds each host-updater request
to its host policy revision. The engine checks that revision under the same
lock used to commit stage, retry and activation grants. `policy off` takes
that lock and returns only after preventing further grants. A policy change
during a download prevents the later stage or grant. Missing, malformed or
unreadable policy fails closed. Manual mode still reconciles already-granted
work and reports its result, but starts no new hop. Remote update controls keep
requiring an enrolled administrator. Only a local Unix-socket caller may change
`HostPolicy`: `mesh update policy show`, `set` and `off`.

**Default.** With no policy file the mode is automatic when the executable is a
stable release build on a Mesh-managed path with a daemon service. Development
and modified builds are always manual.

**Schedule.** In automatic mode `updatenotice` checks every 30 minutes plus up
to five minutes of jitter, down from six hours. The existing
`conditionalTransport` keeps an unchanged answer cheap. The updater also checks
at daemon start and when the step timer shows a clock jump, which is how a
laptop notices a release on waking. Releases published between two checks
collapse into one update.

### Frozen formats and the upgrade map

`mesh-release.json` stays at schema 1 with its current fields and exactly three
artifacts. The journal stays at schema 1. This is what lets today's installed
builds read the activation release. A packaging test fails when the manifest
generator (`internal/release/cmd/manifest`) emits a field that the v0.1.249
decoder rejects.

A breaking change to either format, to the worker protocol, or to the platform
set goes through a waypoint: one anchor that still writes the old format and
reads the new one. Releases after it stop proving transitions from sources
older than the waypoint, and the planner routes those sources through it.

Each release gains one asset, `mesh-upgrade-map.json`, at most 256 KiB:

```go
// internal/release/upgrademap.go
type UpgradeMap struct {
	Schema    int          `json:"schema"`
	Version   string       `json:"version"`
	Manifest  string       `json:"manifest"`  // digest of this release's manifest
	Anchors   []MapRelease `json:"anchors"`   // every anchor, ascending
	Supported []MapRelease `json:"supported"` // every release since the oldest supported anchor
}

type MapRelease struct {
	Version     string    `json:"version"`
	Manifest    string    `json:"manifest"`
	PublishedAt time.Time `json:"publishedAt"`
}
```

The map tells a host which releases are stepping stones and lets status count
how far behind a host is. It grants nothing. Every hop is still admitted by
`Manifest.Allows` on the exact executing digest and by the content-addressed
receipt (`VerifyTransitionReceipt`, `bridge.go:181`). A wrong map can make a
plan fail. It cannot make an untested transition install.

### Supported releases, anchors and the release gate

`scripts/upgrade-policy.json` holds the numbers: sixteen releases at most
between anchors, 24 supported anchors, a 30-day minimum horizon, and the seed
anchors.

- **Anchor.** A candidate is an anchor when the previous anchor was published
  in an earlier ISO week (UTC), or when sixteen releases have been published
  since the previous anchor. The decision is written into that release's map
  and never changes.
- **Direct sources.** A candidate proves a transition, on all three platforms,
  from the newest anchor, from every release after it, and from each of the 24
  newest anchors. That is at most 40 sources per platform against eight today.
- **Supported.** A release is supported when it is one of the 24 newest anchors
  or was published after the oldest of them. At the November 2026 pace that is
  about three months. A supported release reaches the candidate directly, or
  through the first anchor after it, which proved every release since the
  anchor before it.
- **Reachable.** Every other published release keeps a longer chain through
  older anchors and through the eight-release windows published before this
  plan.
- **Seeds.** The first map lists v0.1.249, v0.1.255 and v0.1.257 as anchors,
  plus the release eight before the first policy release, so the old windows
  connect to the new scheme. The seeds age out with the other anchors.

A new tool, `scripts/upgrade_graph.py`, plans the sources and checks the result.
It grows from `design-evidence/analyze.py`. The publisher refuses to publish
when:

1. a direct source lacks a passing receipt on any platform, including a failed
   download;
2. any published release with a manifest has no chain to the candidate on any
   platform;
3. a supported release needs more than two hops;
4. the oldest supported release was published less than 30 days ago;
5. an anchor's manifest is missing or differs from its recorded digest;
6. the manifest exceeds 512 KiB.

Rule 4 exists because a count shrinks in calendar time when releases speed up.
It forces a deliberate change to the policy file and stops the window from
quietly closing again. The job summary prints the oldest supported release, its
date and the longest chain.

Anchor releases and their assets are never deleted. No release script removes
one, and rule 5 catches a removal made by hand.

### Chain planning

`internal/release/plan.go` holds one pure function over a manifest fetcher:

```go
func PlanUpgrade(ctx context.Context, build Build, target Manifest, upgrades UpgradeMap, fetch ManifestSource) ([]Manifest, error)
```

It returns the target alone when the target allows the build. Otherwise it
searches breadth-first over the map's anchors newer than the build, taking the
newest anchor that allows the current position first. It fetches at most
sixteen manifests in 30 seconds, plans at most twelve hops, and checks each
fetched anchor against the digest in the map. Fetched manifests are cached by
version and digest.

For every hop the planner requires an exact transition from the previous hop's
artifact digest and a verified receipt. The host updater also requires the
receipt's rollback proof. `mesh update --check` uses the same planner, shows the
whole chain, and marks a hop that lacks rollback proof as manual only.

Walking anchors is complete for every host that has an updater. Such a host
runs the activation release or a later one, the first anchor after any release
proved a transition from it, each anchor proved transitions from the 24 anchors
before it, and the target proves transitions from the 24 newest. The
eight-release windows published before this plan matter only to the release
gate's reachability check. `Bridge` and its 32-entry history page are deleted
once the CLI preview uses the planner.

### One hop

A hop is today's transaction, unchanged: `accept` re-reads the manifest from
the origin and compares digests, `preflight` checks the executing digest
against the installed file and every worker's protocol, and the helper
activates and verifies.

One thing changes. The helper must be the same executable as the installed
daemon and must be running before a hop starts.

- After a commit, promotion requires only that the new helper image answers
  `update-helper --check-journal`. The test on the old helper's state version
  in `helperUpgradeAllowed` goes away. The helper owns the journal and does not
  own the database.
- Before a hop, the updater compares the helper receipt and the running helper
  process (`updatebootstrap.HelperProcessProbe`) with the installed daemon. A
  stale or stopped helper is promoted or started through the existing
  preparation and restart path while the journal is in a finished phase. If
  that fails, the host reports `attention: update helper` and starts no hop.

So the pair that runs a hop from release S is always S's daemon and S's helper,
and that is the pair CI tests.

### State migration and rollback

Rules for schema changes, enforced by the proofs:

- A shipped migration is never edited, squashed or removed. A candidate must
  open the state of every direct source.
- A migration must leave the database usable by every direct source. A change
  that an older source cannot live with waits until that source is routed
  through a waypoint.
- Rollback restores the executable and never restores a database snapshot. T26
  decided that, and it holds.

`prove-release-transition.sh` gains a rollback leg after the candidate's
restart check: stop the candidate, start the retained source executable on the
candidate-written state, require it to list the retained session, attach,
exchange a token, and keep the recovery record byte-identical.

Forward transition receipts retain schema 1 and their exact existing fields.
The manifest's `proof` continues to address that legacy-readable receipt.
Rollback results live in a separate content-addressed asset, bound to the
forward receipt digest, source and target executable digests, platform and
tested state boundaries. Signed update metadata binds its digest. Automatic
hops require both proofs. The actual v0.1.249 reader must accept generated
forward receipts, and the v0.1.196 helper must accept the frozen journal.

Goose commits each migration separately. The proof injects failure or
interruption before and after every committed migration and exercises
representative writes and changed persistent configuration at every reachable
state. From each state, the retained executable must start and preserve live
session I/O and recovery records. Every intermediate state must remain
compatible, or the migration batch must become atomic. A failing seed needs
a compatible tested path before rollout. Database snapshots are not restored.

### Sessions and services across hops

- Workers own their PTYs and keep their original executable. `verifyHealth`
  fails a hop when any worker's PID or shell PID changed, and that failure
  rolls the hop back.
- The hard link to each replaced executable stays while a live worker runs it
  (`internal/updateinstall/prune.go:20`). A session started three hops ago
  keeps its image.
- Release compatibility includes all retained worker implementations that can
  still run, independently of the direct daemon-source window. CI keeps an
  oldest-worker fixture alive through several daemon and helper upgrades and
  rollback at every hop. An incompatible candidate fails publication. A live
  session must not prevent every later update or require termination to finish
  this plan.
- New sessions stay gated during validation (`internal/updategate`).
- The daemon's own listeners restart with it. A process inside a session, such
  as the PC's model proxy, keeps running and keeps its sockets. Mesh routes to
  it return when the new daemon starts. Unit 8 measures that gap on every
  platform.

### Failure handling

| What happened | What the updater does |
| --- | --- |
| Check, download, space, mount, lock or preflight failed before activation | Retry with backoff from 15 minutes to six hours. The daemon was never stopped. |
| Activation failed and rollback verified | Persist the failed transition's source and target digests and platform. Exclude it from every later plan, including plans for newer final releases. Replan through a proven alternative or wait for one. |
| Rollback failed | `attention`. No further hops. The helper keeps both executables and the journal. Recovery follows `.internal/helper-recovery.md`. |
| A manual operation owns the journal | Wait, and resume when it finishes. |
| A reachable gate reports failure on the target | Hold that release. |

An explicit local retry may clear a named failed transition. A newer final
release does not clear it. Quarantine records survive restarts and remain
until no retained route depends on them; they are not evicted by eight newer
releases. Reports go out through status, below.

### Fleet order

The host info reply gains an optional summary. `DecodeControl`
(`internal/protocol/control.go:376`) tolerates unknown fields, so peers on
older builds keep reading host info.

```go
// protocol.HostInfo.Update
type HostUpdateSummary struct {
	Mode    string            `json:"mode"`
	Phase   string            `json:"phase"`
	Target  string            `json:"target,omitempty"`
	Problem *HostProblem      `json:"problem,omitempty"`
	History []CommittedUpdate `json:"history,omitempty"` // newest 8: version, digest, committedAt
}
```

A gated host reads its gate peers' host info with the daemon's own identity,
through the ordinary authenticated control the dashboard uses. It needs device
access to the peer and no update-administrator grant. For a release R:

- a gate is **verified** when its history shows R committed at least `Soak`
  ago;
- a gate **holds** R when its current problem names R. A problem stays in the
  summary until a later update commits;
- a gate gives **no information** when it is unreachable or runs a build with
  no summary. Once that has lasted `GateWait` without a break, the gate stops
  counting until it answers again.

The target is the newest release that every counting gate has verified and no
gate holds. A gated host therefore installs exactly the releases its canaries
ran, a soak period later. A canary that goes down delays the others by
`GateWait` once and no longer.

For the owner's fleet: the Pi and the VPS have no gates. They are always on, so
they are the canaries. The Mac waits for both with a 30-minute soak. The PC
waits for both with a two-hour soak, so an awake Mac updates before the PC and
a sleeping Mac never delays it. Both use a 24-hour gate wait. A new machine has
no gates.

### Visibility for managing

`mesh update fleet [--json]` reads the host info of every member of the local
saved fleet and prints one row each: version, mode, phase, target, releases
behind, last committed update and current problem. It exits 0 when every member
is current, 2 when a member is pending inside its soak and grace, and 1 when a
member has a problem, has been behind for more than six hours past its soak, or
has been unreachable for a day. The managing chat's hourly check runs it on the
PC. The exit status and the `problems` array are how managing is told.

The dashboard's update attention area (plan 14 unit 5) reads the same summary
in place of the coordinator policy record.

### Offline, sleep and reboot

- **Offline.** A failed check retries after 15 minutes. Nothing else changes.
- **Back after weeks.** The first check plans the chain and runs it. Supported
  releases need at most two hops.
- **Asleep during a hop.** The helper's journal carries the transaction. It
  continues on wake.
- **Reboot during a hop.** The helper resumes its journal after boot, as today.
  The reboot interrupts sessions, and the result reports them as interrupted
  (invariant 5). The updater state is on disk and reconciles with the journal
  at daemon start.
- **Daemon crash between hops.** The next start runs the same step.

### Package-managed and client-only installations

The updater never writes a package-owned executable. On a path that
`ValidateInstallationPath` rejects (Homebrew, `/usr`, Nix, Snap) the mode is
manual, status says "package-managed" with the package manager's command, and
`mesh update fleet` counts the host as behind. A client-only installation has
no daemon to run the updater and keeps its reminder. The fleet's four machines
run the daemon from `~/.local/bin/mesh` or its equivalent; the rollout
inventory confirms that for the Pi, the VPS and the Mac.

### New installations

`scripts/install.sh` and `mesh daemon install` install the update helper with
the daemon, and `daemon installation-check` verifies that it runs. Today the
helper appears only at the first update. A fresh machine has no policy file, so
it is automatic with no gates from its first daemon start.

### Downloads and cache

- **Host.** One hop's archive at a time, at most 128 MiB (`maximumArchive`),
  after the existing 128 MiB free-space and required-mount checks. Planning
  reads at most sixteen manifests of 1 MiB and one map. `update.PruneCache`
  keeps the installed release and the hop in flight and runs after every
  commit. The planning cache keeps only manifests named by the current plan.
  `MESH_UPDATE_CACHE_DIR` and `MESH_UPDATE_REQUIRED_MOUNT` apply unchanged.
- **Rollback images.** Unchanged: the latest previous executable, plus any
  image a live worker still runs.
- **CI.** Source executables are immutable, so the proof jobs cache them by tag
  and platform and download only new ones. Uncached, 40 sources are about 440
  MB per platform. Published manifests are cached the same way.

### The coordinator policy

The consent-bound policy and the host updater would compete for the same
journal. Until the policy is removed, the updater stays idle on a host with an
enabled recipient consent (`automatic-consent.json`). No fleet machine has one.
Unit 11 removes the policy, its CLI and its integration test after the fleet
runs on the host updater. `mesh update` fleet operations, administrator trust
and configuration delivery are untouched.

## The existing fleet's one-time transition

v0.1.249, v0.1.255 and v0.1.257 contain no code that installs a release
without an approval. A published release changes what a machine does only
after that machine runs it. Each of the four machines therefore needs exactly
one approved installation that starts outside its daemon. After that hop it
runs the host updater and needs nothing more.

- **The hop is direct.** The activation release is the first release with
  units 1 to 9. Its manifest lists transitions from the three seed executables,
  with rollback proof. The installed CLI's existing command does the rest:
  `mesh update --local --version ACTIVATION --yes --json`. No intermediate
  version is searched for or installed.
- **What old code does in that hop.** The old CLI previews with its own
  `Allows`, submits a one-host plan to its own daemon over the Unix socket, and
  the old daemon installs or starts its helper, re-reads the manifest with its
  strict decoder, stages and grants. The old helper activates the new daemon
  and judges its health. All of that is code we cannot change, which is why the
  manifest format is frozen and why CI runs this exact path first.
- **Exact pairs.** A hop is run by a daemon and a helper. The PC's pair is the
  v0.1.249 daemon with the v0.1.196 helper, stopped. Unit 8 installs each
  machine's real pair from published archives on its native platform, opens a
  live session, runs the old CLI's command against the candidate, and requires
  the session to survive. The Pi, VPS and Mac pairs come from the read-only
  inventory. An installed executable that differs from its published artifact
  uses the existing `baseline_OS_ARCH_SHA256.bin` path.
- **If a pair fails in CI.** The fix goes into the candidate, where
  compatibility code is allowed. If the v0.1.196 helper cannot judge the new
  daemon at all, a conditional unit adds an `update-helper refresh` command
  that the new executable runs from a side path to promote the helper while the
  journal is committed. Build it only if unit 8 shows the need.
- **The PC.** The PC's administrator policy holds one key, the PC's own
  identity, so no other machine can start its hop. The one command runs on the PC, last, after the
  other three machines have each taken a later release with no action, and
  after managing has agreed to a time.

## Implementation units

Each unit is one pull request with its own independent review. Hours are agent
hours for implementation and one review round, from comparable Mesh work.
Replace each with the measured time when it lands. Until unit 9, the default
mode constant is manual, so intermediate releases change no behavior.

| Unit | Outcome | Depends on | Hours |
| --- | --- | --- | --- |
| 1 | Graph tool, policy file, upgrade map type | none | 1.5 |
| 2 | Publisher proves the supported set and refuses gaps | 1 | 3.5 |
| 3 | Rollback boundaries and separate rollback proofs | 2 | 2.5 |
| 4 | Chain planner | 1 | 2 |
| 5 | Helper stays current and running | none | 2 |
| 6 | Host updater | 4, 5 | 5 |
| 7 | Status, policy and fleet commands | 6 | 3.5 |
| 8 | Installed catch-up proofs in CI | 3, 6 | 5 |
| 9 | Gates, installer default, automatic by default | 7, 8 | 4 |
| 10 | Fleet activation | 9 | 3, over two days |
| 11 | Remove the coordinator policy, write the ADR | 10 | 2.5 |

Units 1 and 5 start at once. Units 2 and 4 start when unit 1 merges. About 35
agent hours in total.

**Unit 1: graph tool and map type.** Files: `scripts/upgrade_graph.py`,
`scripts/test-upgrade-graph.py`, `scripts/upgrade-policy.json`,
`internal/release/upgrademap.go` and its test, fixtures under
`internal/release/testdata/published/`. Tests: the tool reproduces the three
published chains above from recorded real manifests; it reports the missing
v0.1.269 source of v0.1.272; anchor selection is deterministic for a recorded
release list; the map round-trips and rejects unknown fields and oversize
input.

**Unit 2: publisher.** Files: `scripts/prepare-release-baselines.sh` and its
test, `.github/workflows/release.yml`, `scripts/check-packaging.sh`,
`internal/release/cmd/manifest`. Every planned source is required. Source
executables and manifests are cached. The six refusal rules run before
`finalize-release.sh`, and the map is uploaded with the manifest. Tests: each
refusal rule fails publication on a fixture; a failed source download fails the
job; the generated manifest decodes with the v0.1.249 decoder (a frozen copy of
its struct in testdata). `ci.yml` gains a path-filtered pull-request job on the
three native runners that runs the forward proof from the previous release,
the newest anchor and the seeds against a candidate built from the pull
request. This unit's own pull request must pass it for every seed, so the
first policy release cannot fail publication. Measure proof time per source.
Shard the sources across matrix jobs if a platform passes ten minutes.

**Unit 3: rollback leg.** Files: `scripts/prove-release-transition.sh`,
`scripts/test-release-transition.py`, `scripts/assemble-release-compatibility.py`,
`internal/release/bridge.go`, `integration/release_transition_checkpoint.sh`.
The pull-request job from unit 2 runs the rollback leg too. Tests: a candidate
with an unreadable intermediate migration state fails the proof; failure and
interruption at every migration boundary preserve rollback and session I/O;
the actual old receipt reader accepts the generated schema 1 forward proof;
an automatic hop without the separate matching rollback proof is refused.
The rollback leg becomes required for publication only after it passes for
every seed on this unit's pull request. Worker fixtures created by the oldest
retained implementation stay alive across several upgrades and rollbacks.

**Unit 4: planner.** Files: `internal/release/plan.go` and its test,
`internal/cli/update_bridge.go`, `internal/cli/update_readiness.go`; delete
`Client.Bridge`. `mesh update --check` shows the whole chain. Tests on the
recorded manifests: with a map that names v0.1.257, 266, 274, 284, 293 and 304
as anchors and rollback proof not required, v0.1.249 to v0.1.306 plans the
seven published hops; a direct transition plans one hop; a map whose anchor
digest differs is refused; the manifest and time budgets hold; with rollback
proof required, a hop that lacks it is never planned.

**Unit 5: helper currency.** Files: `internal/updateinstall/helper.go`,
`helper_probe.go`, `internal/cli/update_helper.go` and tests. Promotion after
commit depends on the journal check. A new `EnsureCurrentHelper` promotes and
starts a stale or stopped helper when the journal is finished. Tests: a helper
three state versions behind is promoted after a commit; a stopped helper is
started before a hop; a failed start restores the prior helper and reports;
`scripts/prove-helper-recovery.sh` still passes. Diagnose here why the PC's
unit is inactive after boot.

**Unit 6: host updater.** Files: `internal/update/host_updater.go`,
`host_policy.go`, `host_state.go` and tests, `internal/daemon/update.go`,
`internal/daemon/app.go`, `internal/updatenotice/notice.go`. Tests with the
real engine and a fake service: a two-hop plan commits both hops with one
worker identity throughout; a daemon restart between hops resumes; a
rolled-back hop remains excluded when more than eight newer targets still
require it, and a verified alternative proceeds; policy off and revision
changes race download, stage, retry and grant without issuing later work;
manual mode reconciles already-issued work; malformed policy fails closed; a failure
before activation backs off without stopping the daemon; a manual operation in
the journal makes the updater wait; a package-owned path and a development
build stay manual; an enabled recipient consent keeps it idle.

**Unit 7: status and commands.** Files: `internal/protocol/control.go`,
`internal/daemon` host info, `internal/cli/update_policy.go`,
`internal/cli/update_fleet.go`, `internal/tui/dashboard_*`,
`docs/guides/updates.md`, `docs/reference/cli.md` (generated). On a host with
the updater, `mesh update --local --yes` hands the target to the updater, which
runs its plan now. Tests: policy changes are refused from a network peer; a
local run clears a quarantine; `mesh update fleet --json` reports
current, behind, failed and unreachable members with exit 0, 2 and 1; an older
peer without the summary reads as no information.

**Unit 8: installed catch-up proofs.** Files:
`scripts/prove-installed-catchup.sh` and `.py`,
`scripts/fixtures/fleet-baselines.json`, `.github/workflows/ci.yml`,
`.github/workflows/release.yml`. It reuses `scripts/fixtures/published_releases.py`
and `tls.py` for a loopback release origin, as the helper-recovery proof does.
Three scenarios on each native platform:

1. *Legacy pair.* Install each fleet pair from published archives, open a
   session running a small listener, run the old CLI's local update to the
   candidate, and require the same worker PID, shell PID, listener PID and
   terminal I/O afterwards. Then force a failed candidate and require a
   verified rollback with the session intact.
2. *Automatic chain.* Before automatic defaults are enabled, publish an
   updater-capable release that remains manual by default. Install that exact
   published build in explicit automatic mode and run through an actually
   installed intermediate daemon and helper to the candidate. Require both
   hops unattended with the oldest retained worker alive after each. Variants
   interrupt helper replacement, restart between hops, and fail the second
   hop to verify rollback and persistent failed-transition quarantine. Three
   labelled builds of current source may supplement this test.
3. *Activation release forever.* Retain the first published updater build as
   an installed baseline. Its original updater must reach every candidate
   through retained releases and discovery metadata. Include a deliberately
   incompatible future format reached through a compatible waypoint.

The service manager is real: a systemd user manager on Linux runners and the
launchd `gui` domain on macOS runners. Confirm both work on hosted runners
before building the rest. A platform without a usable hosted manager runs the
installed proof in a disposable CI VM with the real native manager. A command
provider is a supplemental test and does not satisfy this gate. Keep a terminal
attached and a service request or stream active across activation, forced
rollback and helper replacement. Verify reconnection, unchanged worker,
shell and listener identities, the route gap, helper process identity,
failed-start restoration and interruption recovery before rollout. The job
joins `Complete CI gate` and runs on pull requests that touch
update, storage, migration, worker, protocol, daemon or release-script paths,
so upgrade breakage is caught before merge.

**Unit 9: gates and defaults.** Files: `internal/update/host_gates.go` and its
test, `scripts/install.sh`, `scripts/install/linux.sh`, `darwin.sh`,
`internal/cli/install.go`, the default mode constant. Tests with three daemons:
a gated host installs only after its gates have soaked; a gate failure holds
the release; an unreachable gate stops counting after the wait; a fresh
install runs a helper and is automatic. This is the pull request whose release
becomes the activation release.

**Unit 10: fleet activation.** The runbook below. No code.

**Unit 11: removal.** Delete only consent-policy-specific code in
`internal/update/automatic*.go`,
`internal/cli/update_automatic.go`, `integration/automatic_fleet_updates.sh`
and its tests and docs sections. Retain `internal/update/joined.go` and the
ordinary coordinator's installation-joining and cancellation tests; those
functions serve manual fleet operations. Add `docs/adr/009-each-host-updates-itself.md`.
Mark plan 14's binary steps closed and turn
`/work/reports/mesh-auto-update/PLAN.md` into a pointer to this file.

## Rollout runbook

The lane coordinator runs this. Every step records its output under
`/work/reports/mesh-auto-update/rollout/`.

**0. Inventory, read-only, any time.** On each machine, through its usual
access: `mesh version --json`; the phase, release and settings of
`update/installation.json`; the helper receipt digest and the helper unit's
state; the executable path; the count of update administrators; live sessions;
free space. The PC is recorded in the design evidence. The result fills
`fleet-baselines.json` for unit 8.

**1. Activation release.** It is published by normal CI. Unit 8 is green for
all four fleet pairs. On each machine `mesh update --local --check --json
--version ACTIVATION` shows a direct tested transition and compatible workers.
`--check` installs nothing.

**2. Pi.** Run the one command on the Pi. Check the daemon version, each
session's PID, a running and current helper, `mesh update policy show`, and
`mesh update fleet`. Then wait for the next published release and watch the Pi
take it with no action. That second update is the proof of autonomy. Measure
the route gap and what an attached remote client sees during the restart. Fix
session or service reconnection failures before continuing rollout.

**3. VPS.** The same, in the root account its daemon uses. The Pi and the VPS
are the canaries from here on.

**4. Mac.** Set its gates: `mesh update policy set --after PI --after VPS
--soak 30m`. Run the one command. Check launchd, the helper and the sessions.
Close the lid across one release and confirm the Mac catches up after waking.

**5. PC, last.** Preconditions: the Pi, the VPS and the Mac have each taken at
least one later release unattended with sessions intact; the PC's check shows
a direct transition and every live worker compatible, including the model
proxy's session and the managing chat's sessions; `/work` is mounted with
space. The coordinator tells managing what will happen (one daemon restart,
its measured length, sessions keep running, attached clients reconnect), what
rollback does, and how to recover if rollback fails (local terminal or system
SSH, `.internal/helper-recovery.md`), then waits for managing's go-ahead and
time. Run the one command on the PC. Set its gates: after the Pi and the VPS,
a two-hour soak, a 24-hour gate wait. Check the daemon, the model
proxy session's PID and replies, the managing chat's sessions, the helper, and
`mesh update fleet` exit 0.

**6. Finish line.** The next release reaches the Pi and the VPS, then the Mac,
then the PC, with no action from anyone, and `mesh update fleet` reports it.
Add the fleet command to the managing chat's hourly check. Then run unit 11.

**Stop conditions.** A failed hop that rolled back stops the rollout at that
machine. Nothing else is touched until the cause is fixed in a new release. A
failed rollback is reported to managing at once. No step is retried by raising
a timeout or editing the journal.

## Non-goals

- A Homebrew, apt or Nix adapter. Package-owned installations are reported and
  left to their package manager.
- Automatic updates for client-only installations.
- A central fleet database or a permanent coordinator.
- Downgrades. A host never installs an older release.
- Pre-release or alternate channels.
- Automatic repair after a failed rollback. That stays a person's decision.

## Risks and decisions

1. **The PC needs one command.** The brief for this design says no manual PC
   update. The PC's installed code cannot start an update by itself, and the
   PC trusts no other administrator, so one PC-local command is unavoidable.
   The runbook makes it the last step, behind three unattended machines and
   managing's go-ahead. The coordinator should confirm this reading with
   managing before step 5.
2. **Removing the coordinator policy** deletes recently merged work (#395 and
   the binary half of #414). It was never enrolled on the fleet, and the goal
   it served is now met differently. This is the largest judgment call here,
   and the reviewer should test it.
3. **Releases are not signed.** The owner's plan says "signed releases". What
   exists is a fixed HTTPS origin plus hashes. With unattended installs, a
   compromised GitHub release reaches the whole fleet without a person in
   between. An Ed25519 signature over the manifest, checked against a key
   compiled into Mesh, needs no dependency and about three agent hours. It
   needs a signing secret, which is the owner's to add. Owner decision.
4. **The seed rollback leg may fail.** Unit 3 proves every reachable migration
   boundary before activation. A failure requires a compatible migration or
   adoption path and prevents rollout until that path passes.
5. **The old helper is the judge.** The v0.1.196 helper must accept the
   activation daemon's health. Unit 8's legacy pair test answers this before
   the rollout.
6. **Hosted runners and real service managers.** Unit 8 starts by confirming a
   systemd user manager and the launchd `gui` domain on hosted runners.
7. **Proof time grows** from 8 to at most 40 sources per platform. Unit 2
   measures it and shards if needed.
8. **Restart frequency.** Releases publish two or three times a day. The
   30-minute check and the PC's two-hour soak collapse bursts. CI and rollout
   must prove client reconnection and service recovery before enabling defaults.
9. **Workers outlive the daemon support window.** Retain their protocol
   compatibility and exercise the oldest live-worker fixture through later
   upgrades and rollback. Publication must fail if that guarantee regresses.

## Evidence

`/work/reports/mesh-auto-update/design-evidence/` holds the manifest fetcher,
the graph analysis, its output for v0.1.306, and the PC's read-only inventory.
No product check ran for this prose-only design. Every path, function and line
cited above was read on main `514700f4`.
