# T26: Update the whole Mesh

Status: implemented after [design review](../evidence/t26-plan-review.md).
See the [update guide](../updates.md) and
[implementation evidence](../evidence/t26-implementation.md) for behavior and
verification. Publication and deployment are separate delivery checks.
Requested on 2026-09-07 after a PC reboot exposed an installed version without
the recovery features already committed to this repository.

## Outcome

Run `mesh update` from one machine to update every member of its configured fleet
to one tested release. Mesh announces available updates, preserves running
sessions, and reports which hosts still need work. Offline hosts remain pending
and retry when they and the coordinator are reachable. Closing the initiating
terminal does not cancel the operation.

The command starts one fleet operation. Hosts do not all restart simultaneously,
and the operation is not an atomic transaction across disconnected computers.

First, [deliver the existing recovery fix](#deliver-recovery-independently)
through the current release and installation paths. That delivery runs
independently of this updater project and does not wait for its milestones.

## User experience

| Command | Behavior |
| --- | --- |
| `mesh update` | Preview and approve the latest stable release for every configured fleet member. |
| `mesh update --check` | Refresh availability and show installed versions, capabilities, and pending work without installing anything. |
| `mesh update --local` | Update this machine only. |
| `mesh update --host NAME` | Update one adopted host. The flag can repeat. |
| `mesh update status [RUN]` | Show persisted progress and the last verified result for each host. |
| `mesh update retry RUN` | Retry unfinished targets using the original approved release. |
| `mesh update cancel RUN` | Cancel pending targets and stop issuing activation grants. Report work already authorized. |

`--all` is an explicit alias for the default scope. `--version TAG` selects an
exact published release. Ordinary updates never downgrade a newer installation.
`--fleet FILE` uses an explicit fleet manifest instead of the saved default.
`--yes` approves the displayed scope for scripts; `--json` produces structured
output. Noninteractive installation requires `--yes` and never reads stdin.

The preview identifies the exact release, included machines, offline machines,
and sessions using old workers. Approval occurs once for the whole operation.
Accepting an update notification opens this same preview.

Example notification, with illustrative versions and host counts:

```text
Mesh v0.2.0 is available. 3 machines need updating.
[Review update]  [Remind me tomorrow]  [Skip this version]
```

The daemon checks in the background at startup and every six hours, with jitter,
a short timeout, and cached conditional requests. An explicit check refreshes
the cache. A failed check retains the previous result and its timestamp.
Attachment never waits for a release server.

Without a local daemon, an interactive picker starts the same release check
asynchronously. Direct interactive attachment schedules a detached check with
a five-second total deadline when the cache is due, then attaches immediately.
Use the existing cache for the current invocation; a later invocation displays
the refreshed result. Record failed-attempt timestamps to avoid repeated checks
during an outage, and allow only one cache refresh per user at a time. These
checks never install a daemon. Noninteractive hooks, redirected output, and
JSON commands do not schedule them. Explicit `--check` may wait for its bounded
refresh because checking is the requested operation.

Show the notification in the full picker and compact window picker. Interactive
CLI entry can print a short cached notice before terminal attachment. Never
insert notices into an attached PTY, provider conversation, command stdout,
JSON output, or noninteractive hook invocation. Snooze and dismissal persist
per release. An unfinished rollout remains visible even when its release notice
was dismissed.

Example result:

```text
pc       updated          daemon v0.2.0; 4 sessions use older workers
pi       updated          daemon v0.2.0
laptop   pending offline  will receive v0.2.0 when reachable

2 of 3 machines updated. 1 pending. Running sessions preserved.
```

Exit 0 means every selected installation has been verified at the target
release or reported as `already newer` with compatible capabilities. A newer
incompatible build requires intervention. Exit 2 means work remains pending.
Exit 1 means a target failed or requires intervention. Older workers are reported separately from installation
success; their missing capabilities must never be described as installed.

## Verified starting point

- [CI](../../.github/workflows/ci.yml) watched `master` while the GitHub default
  branch and source commits were on `main`. A local correction to `main` was
  made during this investigation. It has not been pushed or run in GitHub.
- [Release publication](../../.github/workflows/release.yml) runs on version
  tags. GitHub's latest release was `v0.1.38`, published September 3. Recovery
  commits `eb7d26e` and `3c065d9` landed September 5 without a containing release.
- [Host records](../../internal/cli/config.go) contain pinned identities and
  endpoints, without SSH credentials. [Adoption](../../internal/bootstrap/running.go)
  supports machines without SSH, so an SSH loop cannot be the update mechanism.
- [Release resolution](../../internal/bootstrap/release.go) verifies platform
  archives and checksums, but prefers the invoking executable. Updating must
  select the approved release explicitly instead of copying an old client.
- [Service assets](../../scripts/install/assets/mesh.service) preserve workers
  on daemon restart. [Existing workers retain their version](../terminals.md)
  and cannot gain checkpoint code merely because the daemon was replaced.
- [Host information](../../internal/protocol/control.go) lacks build and updater
  metadata. [Target identity checks](../../internal/cli/remote.go) do not
  authenticate a requesting update administrator. The WebSocket session path
  currently relies on tailnet access.

## Recommended architecture

The initiating machine's daemon owns the operation. It persists the selected
release and a snapshot of the selected hosts before acknowledging approval.
Targets own their installation transactions. The CLI submits one operation and
observes it; it does not hold the update process open.

For a client-only or legacy-local installation, the preview includes the
one-time local daemon and helper setup needed to coordinate the operation.
After approval, persist that plan under the existing Mesh state directory
before bootstrap. Install the supervised helper first, then the current daemon;
the daemon resumes the approved plan from disk. This initial local setup is an
explicit exception to updating the coordinator last. The same journal resumes
after reboot. `--local` on a client-only installation can update just the CLI
through the helper without adding a hosting daemon.

For that client-only local operation, the helper owns progress and restart
reconciliation. `status`, `retry`, and `cancel` use its local journal through the
same update service contract. Fleet coordination still uses a daemon; its
one-time setup is included explicitly in the preview even with `--host` scope.

## Fleet membership

Store a versioned `fleet.json` beside the existing host configuration. It names
the fleet and contains a revision, pinned host identities, aliases, endpoints,
platforms when known, and explicit routing dependencies. The file describes
update scope; it grants no access to a member. An address-book entry alone
does not establish that the full fleet has been inventoried.

On first use, the interactive update preview seeds an editable candidate list
from this machine and adopted hosts. Tailnet discovery can suggest additional
Mesh hosts, but it cannot silently add or trust them. The user completes the
list, including offline members, and reviews it in the same update approval.
Save that exact membership with the operation. A script without a saved fleet
must supply `--fleet FILE` or narrower `--local` or `--host` scope. `--yes`
cannot treat an unreviewed address book as the whole fleet.

The same manifest can be copied to another client or supplied with `--fleet`.
Use its fleet name, revision, and membership digest in the preview and report.
There is no automatic manifest replication in this task. A locally added host
does not silently change a saved fleet. An update preview shows adopted hosts
outside the fleet and lets the user revise membership before approving a new
run. A new revision cannot change an already approved run.

Snapshot every member, deduplicated by identity, when approving the operation.
An unreachable member stays in the result. A member lacking a verified identity,
platform, or update authorization is listed as requiring enrollment or preflight;
it is never omitted. Existing address-book entries can supply connection details
for the same pinned identity. A route dependency missing from the manifest
blocks that affected activation until its relationship is resolved. Reject
cyclic dependencies before granting any activation.

The initial inventory for this incident is not complete: this PC's address book
contained only `omarchy` when inspected. The first delivery track below must
establish the actual intended machine list before claiming fleet coverage.

## Durable rollout

The operation records its coordinator identity, so another authorized client
can query its status through that coordinator.

The coordinator stores an outbox for hosts that have not acknowledged the
operation. It retries with bounded backoff when connectivity returns. Targets
with a persisted activation grant can finish while the coordinator is unavailable.
Staged targets without a grant wait. Undelivered work
waits for that coordinator to return; this design has no automatic coordinator
election. The UI shows that distinction.

Resolve `latest` once. Persist the release tag, source commit, manifest digest,
and platform artifact hashes. An offline host returning after another release
is published still receives the release approved for its operation. Changed
bytes under an existing tag cause an error. A newer installed version is never
replaced by a stale queued request.

The coordinator updates one suitable remote host first, verifies it, and then
updates independent hosts with a concurrency limit of two. Routing hosts follow
the hosts that depend on their connectivity. The coordinator updates itself
last among currently reachable hosts. Use the coordinator as the first target
when it is the only reachable member. Offline members stay pending. If that
ordering conflicts with an explicit routing dependency, block the affected
activation and report the dependency rather than dropping either rule.

Staging acceptance and activation permission are separate durable events.
An activation grant binds the operation, target identity, release digest, and
expected target generation. Journal each grant before sending it. The target
journals its acceptance before stopping the daemon or switching executables.
Retries return the same grant receipt. At most two issued grants may have
unresolved results, including grants whose delivery acknowledgment was lost.

Failure or cancellation durably stops new grants. Already issued grants may
arrive or complete after the stop, so report them as authorized work until their
targets respond. Cancellation of staged work takes effect when the target
acknowledges it before accepting a grant for that generation. The target's
generation check serializes that race. Cancellation is not a rollback request.

| Durable target state | Permitted next work |
| --- | --- |
| Accepted | Download and verify the pinned release. |
| Staged | Wait for an activation grant or acknowledge cancellation. |
| Granted | Persist activation intent and finish independently of the coordinator. |
| Validating | Preserve existing attachments; gate new workers until success or rollback. |
| Committed | Release the creation gate and report the verified build. |
| Rolled back | Report the failure and verified restored build; require explicit retry. |
| Cancelled | Reject late grants for that cancelled generation. |

## Activation and rollback

Each target uses a small service-managed update helper that survives the daemon
being replaced. The helper owns a durable journal and the one installation lock
for that host. Its steps are:

1. Verify the exact release, supported platform, writable installation, free
   space, current version, and compatibility with live workers.
2. Stage verified artifacts and preserve the previous executable and necessary
   service configuration. Keep existing flags, environment, ports, drop-ins,
   identity, provider settings, and state paths.
3. Persist the activation grant, intent, and creation gate. Stop only the daemon
   and wait for it to exit, preserving detached workers. Then atomically switch
   the executable and start the candidate daemon. Stage the final rename on the
   executable's filesystem. Do not replace the executable while the old daemon
   can still accept session creation requests.
4. Verify the running daemon's identity, build, session inventory, and ability
   to communicate with existing workers. A downloaded file or active service
   alone is insufficient evidence.
5. Commit success and release the creation gate, or stop the failed candidate,
   restore the previous executable, and restart it when the health deadline
   fails. Verify the restored daemon and existing workers before recording
   rollback success, then clear the gate for the restored installation. Persist
   the result for the coordinator to collect.

The helper resumes its journal after a host reboot, including interrupted
activation or rollback. If rollback itself fails, retain both binaries and the
journal, report `rollback failed`, and require intervention. Do not report a
healthy restored installation based only on a successful file rename.

### Compatibility before state mutation

Each release manifest declares a compatibility contract containing state-format
read ranges, formats it may write, worker protocol versions it accepts and emits,
the helper journal version, and tested source-to-target transition receipts.
State formats include the database, recovery records, and persistent configuration.
A transition receipt identifies exact previous and target artifact digests for
each platform and the proof that the retained binary can reopen candidate-written
state. A promise of compatibility with an adjacent version is insufficient for
a host that skipped several releases.

Preflight reads the actual state versions without opening a store that runs
migrations. Match the executing and installed build digests, live-worker
protocols, and helper journal to an eligible transition. The target must read
current state, and the retained binary must read and write the state the candidate
may persist during validation. Unknown compatibility blocks activation before
any migration or executable switch. A legacy or modified build is eligible
only through a transition tested against its exact digest; a version label
alone cannot establish this. The initial manual delivery track obtains that
evidence separately where the installed legacy binary lacks reporting support.

Release acceptance exercises the transition on isolated state: run the candidate
migration and representative writes, then start the retained previous binary
against that result and verify sessions and recovery records. Fail publication
of automatic-update eligibility if this proof fails. Never restore an old live
database snapshot over newer session data.

### Gate new workers until commitment

The activation marker blocks new session creation and recovery across daemon,
SSH, local CLI, and worker startup entry points until the helper commits success.
This includes legacy clients invoking the newly installed executable directly.
The candidate still permits attachment and I/O to existing compatible workers.
Return a typed temporary-unavailable result for blocked launches so callers can
retry without duplicate sessions. Persist the gate across candidate restarts.

Gating prevents new-version workers from being stranded under the previous
daemon after rollback. Check worker startup as well as daemon requests; otherwise
a direct local launch can bypass the daemon gate. A fresh smoke-test worker must
use an isolated state directory. Existing workers keep their processes and
checkpoints throughout the transaction.

Install and start the first helper before replacing any legacy daemon. The
currently running helper retains responsibility while staging a replacement
helper. Switch the service's helper path only after daemon health passes, then
verify the replacement helper can read and resume the journal before retiring
the previous one. Retain the previous helper until the transaction commits.
Helper changes must preserve the journal format required for rollback.

Identical requests return the existing operation or target receipt. A request
for the same target release joins existing work and adopts its existing grant
status; it cannot issue a second grant. Conflicting target versions return an
explicit conflict. Per-target generations fence stale coordinators.

## Authority and artifacts

Update messages use the existing Mesh transport, with explicit administrator
authorization at the update-control boundary. An adopted host record alone
does not authorize those controls.

Reuse Mesh identity keys and key parsing. Enroll administrator public keys in a
separate target update policy through an owner-controlled installation path.
Requests sign a domain-separated payload containing target identity, coordinator
identity, operation ID, exact release digest, target generation, and a fresh
challenge. Targets verify authorization before staging. Responses bind the
receipt and executing build to the pinned target identity. Replay returns the
existing receipt or a stale-generation error.

This controls who can invoke the update API. It does not create a privilege
boundary against someone who already has arbitrary command execution as the
same OS user. That user can replace binaries and policy files. Legacy Mesh
sessions currently rely on tailnet access for that execution authority. The
bootstrap path below inherits that trust model and says so in its preview;
it must not claim independently authenticated ownership. General session-access
hardening is a separate change, not a hidden prerequisite for updating.

The remote API accepts an approved official release descriptor, not arbitrary
shell commands, paths, or download URLs. Extract the existing bounded archive,
platform, and checksum verification into shared release code used by bootstrap
and updating. Publish release metadata for all supported platforms before a
release is advertised as available. Use the official HTTPS release origin and
freeze its verified artifact hashes for the operation. Both coordinator and
target verify the manifest through the fixed official HTTPS origin; a caller's
proposed hashes are not proof of publication. The manifest binds tag, source
commit, compatibility contract, and the complete supported platform set.

Before issuing the first activation grant, the coordinator caches and verifies
all supported platform artifacts for the approved release. There are currently
three release targets. This also covers an offline member whose platform has
not yet been observed. Pin those bytes and the manifest while any target is
pending or its cancellation has not been acknowledged. Cache eviction cannot
remove pinned data. Preserve target rollback binaries until their transactions
commit or rollback is verified.

A target can fetch artifact bytes from the coordinator after independently
verifying their expected hashes in the official manifest. Publisher policy
retains published manifests at immutable version URLs. If neither a previously
verified manifest nor its official origin is available, the target reports
`release metadata unavailable` and waits. If all verified copies of an artifact
are lost, report `artifact unavailable`. Neither condition silently selects a
new release or reports successful catch-up.

Cache and staging paths are configurable. On this installation, use
`/work/cache/mesh` and `/work/tmp` after mount and space checks. A missing
configured data mount is an error, not a reason to download onto `/`. Small
journals remain beside existing Mesh state. Other hosts keep their configured
data locations. Preserve existing directory ownership.

## Live sessions and the first update

Report the CLI build, installed executable build, executing daemon build, and
worker build or capability for each session. Legacy workers report `unknown`
when they cannot prove a version. Distinguish an older worker that remains
compatible from one missing a requested feature such as crash recovery.

Never kill sessions to make an update report green. A replacement daemon must
continue serving supported old workers. If compatibility is unavailable, block
activation and name the affected sessions. Offer recovery setup and explicit
conversation capture where supported; installing a binary alone does not prove
that provider hooks are configured or trusted. Old workers without checkpoint
support need a newly created or explicitly recovered session to gain it.

The first updater-capable release needs a bootstrap path because installed old
daemons cannot understand the new protocol. Include that path in this task:

- Install the first new client through the existing installer or its package
  manager, then let `mesh update` inventory and migrate the remaining hosts.
- Prefer a bounded legacy bootstrap through an existing Mesh session, running
  a fixed installer for the exact approved release. Persist bootstrap intent,
  run independently of the attaching client, seed the initiating administrator
  key, and verify the new daemon after reconnect. This uses the existing ability
  to run a command and requires a real old-version compatibility test. It is an
  use of the existing tailnet-authorized command channel, not proof of an
  independently authenticated owner. The preview identifies this bootstrap
  route before the operation is approved.
  An operation-specific marker and installation lock let retry discover work
  whose session acknowledgment was lost. The installer independently verifies
  the pinned artifacts and enrolls authority only when no policy exists; it
  never replaces existing administrator policy.
- Where a usable SSH installation route is explicitly configured, it is an
  alternative bootstrap path. Do not infer an SSH user from a Mesh endpoint.
- If neither route works, report `bootstrap required` with the exact host and
  installation command. Do not label that host updated or silently omit it.

Respect package ownership. Use an adapter that can install the exact approved
release for supported package-managed installations. If the package manager
cannot do that, show the required one-time migration to a Mesh-managed install
in the preview. Never overwrite a package-managed executable behind its back.
The client-only helper path remains available without a hosting daemon.

## Proposed internal shape

These are design signatures, not new exported code. Validated release and host
identities have private constructors; wire types stay at the protocol boundary.

```go
type Release struct {
	tag           string
	commit        string
	manifest      Digest
	artifacts     map[Platform]Artifact
	compatibility Compatibility
}

type Compatibility struct {
	StateReads        map[StateKind]VersionRange
	StateWrites       map[StateKind]FormatVersion
	WorkerReads       []ProtocolVersion
	WorkerWrites      ProtocolVersion
	HelperJournal     FormatVersion
	TestedTransitions []TransitionReceipt
}

type Fleet struct {
	Name       string
	Revision   uint64
	Membership Digest
	Members    []Target
}

type Plan struct {
	ID          PlanID
	Coordinator HostID
	Release     Release
	Fleet       Fleet
}

type ActivationGrant struct {
	Run        RunID
	Target     HostID
	Release    Digest
	Generation uint64
}

type Service interface {
	Check(context.Context, Scope, ReleaseSelector) (Offer, error)
	Start(context.Context, ApprovedPlan) (RunID, error)
	Status(context.Context, RunID) (Report, error)
	Retry(context.Context, RunID) error
	Cancel(context.Context, RunID) error
}
```

`Check` resolves the immutable offer. `Start` consumes that exact approved plan
and durably accepts responsibility before returning. Neither the CLI nor the
picker sequences download, installation, or rollback calls.

| Owner | Responsibility |
| --- | --- |
| `internal/release` | Shared release selection and verified artifacts, extracted from bootstrap. |
| `internal/update` | Fleet snapshots, plans, coordinator outbox, activation grants, receipts, authorization, installation journal, and reconciliation decisions. |
| `internal/daemon/update.go` | Startup reconciliation and parsed update controls. |
| `internal/protocol/update.go` | Bounded wire messages and additive build/capability fields. |
| `internal/cli/update.go`, `internal/tui` | Commands, notification, preview, and progress using the same operation. |
| `scripts/install`, update helper | Platform service integration, atomic activation, and rollback. |

A coordinator journal and target journal have different owners. They exchange
receipts rather than sharing files. The public service hides installation and
retry complexity behind a small interface, following boundary discipline and
idempotent operation design.

## Design comparison

Two candidates were evaluated. The chosen design uses the initiating daemon as
a durable coordinator. It incorporates independent target reconciliation from
the alternative design, so accepted installation work survives connection loss.

The alternative assigns a permanent fleet authority and makes every host poll
its desired release. That supports central policy but requires selecting and
enrolling a permanent authority before the first update. The existing local
address books do not supply that fleet model. Retain the smaller operation-owned
model for this task, accepting that undelivered work waits for its coordinator.
The revision adds an explicit membership manifest without adding a permanent
fleet server or automatic membership replication.

A CLI-owned SSH loop also loses because SSH-free hosts and terminal loss remain
the caller's problem. Independent polling of `latest` on every host loses the
single approved release and coherent fleet result. Neither closes this incident's
delivery gap.

## Deliver recovery independently

Start a delivery track for the recovery code that already exists. It does not
depend on the updater implementation, update notifications, automatic publication,
or general package-manager adapters. The updater track can proceed in parallel.

1. Establish the intended host inventory with identity, platform, installation
   route, running build, and session-worker capability. Keep offline and unknown
   hosts visible. Do not infer the fleet from this PC's single address-book entry.
2. Validate a release candidate containing the recovery fixes against the
   existing release gates and retained recovery checks. Fix the CI branch and
   its contract check now. Resolve gate failures without weakening the gates.
3. Publish that tested candidate using the existing tagged-release path. A new
   automatic publisher is not required to deliver this fix.
4. Deploy through existing installation routes, preserving service configuration
   and active workers. Verify the executable and running daemon separately on
   every intended host, and record pending hosts for follow-up.
5. Prove recovery using newly created workers, durable checkpoints, and an
   explicitly captured provider conversation. Existing legacy workers remain
   listed as missing capture until replaced through an explicit user action.
   Do not claim physical power-loss acceptance from a process-kill test.

The deliverable is a host-by-host evidence report distinguishing implemented
source, published release, installed executable, running daemon, and actual
recovery capability. This section schedules that work; revision of this plan
does not constitute publication, deployment, or recovery verification.

## Updater implementation sequence and acceptance

1. **Version and release contract.** Report executing builds and capabilities,
   add the compatibility contract and exact transition receipts, and extract
   shared artifact verification. Reserve the
   `update` command name and handle any existing host alias collision explicitly.
   Prove legacy peers remain readable and unknown versions stay unknown.
2. **One-host transaction.** Implement the updater helper, durable journal,
   restart, health verification, and rollback on Linux and macOS. Exercise real
   service managers in disposable VMs. Prove live worker PIDs and terminal I/O
   survive both successful updates and rollback. Prove that all new-worker
   entry points honor the validation gate and that the retained binary can
   reopen candidate-written state across every advertised transition.
3. **Fleet operation.** Add authorized Mesh controls, coordinator persistence,
   reviewed fleet manifests, frozen membership, activation grants, ordering,
   retry, cancellation, and legacy bootstrap.
   Prove the operation continues after the initiating CLI exits and resumes after
   coordinator reboot. Cover mixed platforms and a host without SSH.
4. **Command and notification.** Wire the shared preview and progress flow into
   CLI and both pickers. Prove cached checks never delay attachment or alter
   child-terminal bytes. Test snooze, version dismissal, JSON, and partial exit
   results. Include client-only refresh, failed-attempt throttling, and the
   helper-owned local operation without a daemon.
5. **Publish and verify the updater.** Ship the first updater, bootstrap the
   intended fleet, and record executing versions
   and recovery capability per host. Separate `implemented`, `published`, and
   `verified on hosts` in delivery evidence.

Retain `scripts/check-updates.sh` as the repeatable acceptance entry point.
It must cover corruption, wrong platform, unauthorized and replayed requests,
concurrent coordinators, an offline host returning after a newer release appears,
failed canary with another target staged or granted, cancellation racing a lost
grant acknowledgment, missing data mount, disk full, package ownership, and
process death at each journal transition. Test same-user legacy bootstrap under
its stated trust model. Test missing release metadata, pinned-cache retention,
client-only notification, and divergent local address books using one fleet
manifest. Exercise every launch path during validation and verify post-rollback
state with the retained binary. Use fault injection and real subprocesses for
transaction checks. VM tests establish reboot and service-manager behavior
separately.

## Publication must advance source history

Release automation is a separate delivery improvement and can run alongside
the updater milestones. A successful complete CI run on
`main` should publish one stable patch release for its exact source SHA, with
explicit version metadata for larger version changes. One aggregate gate must
require every CI job to succeed. Exclude generated Cask-only commits from release
creation to prevent a release loop, and never advertise a partial artifact set.

Use one publication lock and a durable reservation containing source SHA and
version. Check out the successful run's exact SHA and verify its repository and
`main` origin. Under the lock, compare that SHA with the last published stable
source before allocating a version:

- An equal SHA or an ancestor of the published source is a successful skipped
  run. A newer version must never point back to superseded source.
- A descendant is eligible once all its gates have passed.
- A divergent history blocks automatic publication until an explicit release
  decision resolves it.

Create the tag and draft for the reserved SHA. A publication retry resumes the
same reservation; it does not allocate another version. Recheck ancestry under
the lock before making the complete release public. Treat an already published
matching reservation as success. If its source has been superseded, leave it
unpublished and record the skip. Initialize the legacy baseline from the last
stable release's peeled tag when it has no manifest.

Invoke validation and publication jobs explicitly in the gated workflow. Do not
depend on an automated tag push starting another workflow. Never substitute the
current branch tip for the tested SHA. Acceptance must publish descendant B
before delayed ancestor A completes, then verify A is skipped and `latest`
still points to B. Also test equal-SHA retries, divergent history, and failure
between tag reservation and release publication.
Add a packaging contract check for the actual default branch so the `master`
mistake cannot recur unnoticed. Publishing makes the update available; the
user's fleet approval still controls installation.

Completion requires a real rollout report listing every selected host as
verified, pending, failed, or requiring bootstrap. Recovery protection requires
separate evidence of worker checkpoint capability and acknowledged conversation
capture. A green build, a release tag, or replacing the executable is insufficient.
