# Update Mesh

Run `mesh update` to review this machine, or the machines in an explicitly saved fleet. Mesh
pins one release for the operation and keeps progress on the coordinator. You
can close the command while it runs. Offline machines remain pending and retry
the same release when they return.

```bash
mesh update --check
mesh update
mesh update status
mesh update status RUN
mesh update retry RUN
mesh update cancel RUN
```

With no saved fleet, the interactive command and `--check` review this machine
only. They never create a fleet from address-book entries or save one during
approval. Use an explicit fleet file to choose a group, including offline members.
An address book on one machine does not establish the membership of your mesh.

## Set this machine's automatic update policy

Automatic host updates currently default to off. Run these commands on the
machine whose policy you want to change:

```bash
mesh update policy show
mesh update policy show --json
mesh update policy set --expected-revision 0
```

Use the revision from `policy show`. Each successful change increases it.
If another command changes the policy first, read the policy again before
retrying your change.

To wait for other machines to update first, name each predecessor and the
time to wait after its update:

```bash
mesh update policy set --expected-revision 1 --after canary --soak 30m
```

To stop granting automatic work, run:

```bash
mesh update policy off --expected-revision 2
```

An update that already has permission to install may finish, including recovery.
Turning the policy off preserves its predecessor list and wait time.
Policy changes require the machine's owner identity through its local Unix
socket. A network connection cannot change the policy, including a connection
from an enrolled update administrator. Run the command on the destination
machine when changing a remote machine's policy.

To release update history retained for a machine that no longer needs it, run
this command on the machine keeping that history:

```bash
mesh update policy forget DEPENDENT_ID
```

Use the dependent's exact public Mesh identity. The command releases that
machine's retention record. A later evidence read can register it again.
The command leaves the update policy unchanged.

## Check automatic updates across the fleet

```bash
mesh update fleet
mesh update fleet --json
mesh update fleet --details
```

The command reads every member of your saved `fleet.json`, including offline
members. Each responding machine supplies its own version, installation kind,
policy, update phase, verified target, waiting reason, and last pinned commit.
This read requires ordinary device access. Update-administrator enrollment alone
does not grant it.

The exit status describes the whole fleet:

- `0`: Every machine has a managed installation, automatic updates enabled,
  and a recent verified check showing it is current.
- `2`: An update or waiting period is pending, or a machine with a verified
  reply within the last day is temporarily unreachable.
- `1`: A machine needs attention, has updates off, lacks a managed installation,
  has waited more than six hours beyond its configured wait, or has been
  unreachable for a day. Unknown status also returns `1`.

Package-owned, client-only, and development installations do not count as current.
A peer without an update summary reports unknown and may need adoption.
Missing latest releases, waiting times, commit pins, or release counts stay
unknown. Mesh does not infer a release count from version numbers.

The JSON result keeps diagnostic identifiers and has a `problems` array for
machines requiring attention. The dashboard uses the same host summaries for
update attention. Its configuration-delivery status remains separate.

## Choose the machines

```bash
mesh update --all                 # the explicitly saved fleet
mesh update --local               # only this installation
mesh update --host server --host laptop
mesh update --fleet ./fleet.json
mesh update --version v0.2.0       # pin a published version explicitly
mesh update --fleet ./fleet.json --yes --json
```

`--yes` requires a saved fleet, an explicit fleet file, or a narrower selection.
It does not create a fleet from whatever happens to be reachable. `--check`
prints the selected scope and release without installing it. `--details` shows
release identifiers, build metadata, and update checks. `--json` preserves
machine-readable identifiers and the original rejection causes.

A missing local daemon still requires verification of the executing CLI and its update
history before offering helper installation. Unknown legacy sources and failed
authentication checks cannot authorize bootstrap. Unreachable members stay in
an explicitly selected fleet for later inspection; authorization refusals and
responding peers with invalid messages block approval.

The default fleet file is `fleet.json` beside `hosts.json`. `MESH_CONFIG_DIR`
changes this directory. A fleet lists stable Mesh public identities and explicit
connection addresses. Names come from the destination declaration and stay out
of fleet membership and saved operation records. Human output marks retained
names as "cached name" when the current output has no fresh verification of the
owner's declaration. Update states such as pending or failed describe the update
operation separately. A bare-name selection requires a fresh authenticated reply;
an exact ID can select an offline member:

```json
{
  "version": 1,
  "name": "personal",
  "revision": 1,
  "members": [
    {
      "id": "MESH_HOST_PUBLIC_ID",
      "endpoint": "ws://server.example.ts.net:7337/mesh",
      "platform": { "os": "linux", "arch": "amd64" }
    }
  ]
}
```

Replace the placeholder with the host's real Mesh identity. Increment `revision`
when changing membership. A host's optional `dependsOn` array lists the public
identities of routers or relays it needs. Mesh updates that host before those
dependencies. The coordinator updates after the other eligible machines.

Use `--coordinator MACHINE_NAME_OR_ID` when starting or inspecting an operation on another
adopted host. Starting a fleet operation without a local daemon includes its
one-time setup in the approval preview. The independent helper starts the
coordinator and resumes the saved operation if the command closes during setup.
For a client-only installation with no local daemon, `--local` installs the
update helper without adding a hosting daemon.

## Update one managed machine through verified steps

```bash
mesh update --local --check
mesh update --local --yes
mesh update status --local
mesh update retry --local
mesh update cancel --local
```

The local daemon verifies its executing image and the selected release, then
plans the next supported step. The preview shows the source, next verified
release, and selected destination. It can use intermediate releases to reach newer release formats safely.
`--details` and `--json` retain the exact metadata, manifest, binary, and proof
identifiers.

`--check` reads the real planner. It does not install a helper, reserve an
update, stage a binary, or approve installation. An already-current result
requires verified source and release evidence. Approval saves the exact release
returned by that preview, along with the actual request and generation.
Closing the command leaves the approved target with the daemon and helper.
Each later step is verified before installation, and a newer release cannot
change the approved destination.

These commands keep the automatic policy, predecessor list, and waiting period
unchanged. An owner can approve one explicit update while automatic updates are
off. Cancellation or a policy revision change stops future acceptance. Work
already granted may finish, including recovery. A retry deliberately approves
the saved next edge using the current installation and intent revision.

Only this machine's owner through its local Unix socket can plan or submit a
local target. Package-owned and development installations need explicit
handling before they can use this path. Run `mesh daemon installation-check`
to inspect the installation.

Ordinary saved-fleet operations keep their coordinator and `RUN` commands.
Those operations require a tested direct transition. A host without the local
updater keeps its existing supervised bootstrap path. Authentication,
installation recovery, and retained-session checks still block unsafe approval.

## Approve an administrator

Current daemons accept signed update controls from their own identity and from
explicitly enrolled administrators. On each target, enroll the coordinator's
public Mesh identity:

```bash
mesh update trust COORDINATOR_PUBLIC_ID
```

Existing update administrators have update-only network authority. Enrolling an
administrator does not grant session controls, SSH access or service management.
Use `mesh device approve` locally on each destination for that full daemon-account
grant. Removing a device grant leaves a separately enrolled update administrator
able to reconnect for signed update controls; the full-device connection retires.
Each newly approved key line receives a unique grant-incarnation comment in
`authorized_keys`. Connections capture that line identity from the published
policy without taking the writer lock. Revoking and then reapproving the same
key creates a fresh incarnation: old control and SSH attachments retire even if
no poll sampled the absent membership, and
new connections use the new grant. Repeating approval of an existing full grant
preserves its incarnation; another device mutation leaves it unchanged. Use
`mesh device revoke` for retirement. Replacing policy files with an identical
earlier copy can restore the same line identity and is an owner-account action.
Edit the destination's local administrator policy when withdrawing that authority
too.

## Cross the control-authentication cutover

The TLS peer is an Ed25519 key, verified by possession during TLS 1.3 and matched
to an owner-controlled pin or grant. Certificate names, public CAs and certificate
dates do not grant or remove Mesh authority. An encrypted acceptance byte lets
the client observe the server's full-device or update-only grant before a dial
succeeds. Ordinary clients reject update-only approval before sending controls;
the signed updater opts into that narrower scope.
Handshake admission has a five-second deadline and a 64 KiB inbound budget;
reconnections perform a fresh handshake with session tickets disabled.

Grant mutation takes an exclusive lock and frame admission reads under a shared
lock. Later frames are denied once revocation publishes; already admitted work
may finish. A 250 ms current-grant check also closes idle control and SSH
connections. This retirement is polling-based, so scheduler or storage delays can
postpone socket closure; frame admission still checks the current grant. Revoking
a key does not signal its retained worker processes.

The control protocol requires authenticated peers. There is no raw signed-update
fallback. During this cutover, mixed-version network sessions, service controls,
certificates, wake requests and fleet update reconciliation can fail. Local Unix
controls and ordinary service URLs remain available. Update each host
independently through system SSH to keep that interval short.

For the omarchy/macbook-air/pi/vps fleet, use this account-local sequence:

1. Keep authenticated system SSH available. Record each daemon account, stable
   Mesh identity and saved destination pin. The VPS daemon runs as root; full
   device approvals there require `--allow-root`.
2. Run `mesh update --local --version RELEASE --yes` on omarchy locally, then
   run the same command independently over system SSH on macbook-air, pi and
   vps in each daemon's own account. Do not select a remote `--coordinator`.
   With the default coordinator, the old CLI submits its one-host plan to its
   own daemon's Unix socket. That daemon stages the approved release, and its
   independent helper replaces only that installation. The new daemon resumes
   the saved local operation over the same Unix endpoint. Cross-host Mesh
   controls are unnecessary. Retained PTY workers keep their original PID and
   executable throughout daemon replacement.
3. Read each identity with `mesh device identity --json` through that trusted
   account-local path. Preserve existing saved pins and stop if a key changes.
   On each destination, run `mesh device approve -- SOURCE_ID` for every device
   allowed to control that daemon account, including daemon publishers,
   certificate distributors and wake witnesses. On a root destination use
   `mesh device approve --allow-root -- SOURCE_ID`. Keep each account's key in
   its own state directory. Discovery supplies addresses, never approval.
4. Enroll the selected fleet coordinator separately on every target with
   `mesh update trust COORDINATOR_ID`. Existing administrator grants remain
   update-only. Record trusted destination identities and reachable addresses
   in the controlling accounts' host books and the approved fleet file. A Unix
   endpoint identifies only the account-local daemon.
5. After all four daemons run the authenticated release, verify authenticated
   reconnection and reattach retained sessions from an approved device. Run a
   daemon-owned fleet update using the explicit approved fleet and release.
   Coordination can then use omarchy again; no temporary coordinator or bridge
   release is needed for this cutover.

A pin or grant failure is repaired through authenticated system SSH or a
local destination command. Never reopen raw network controls for recovery.

The disposable cutover proof builds the pre-authentication CLI and daemon, plus
two authenticated patch builds, and uses four independent Linux installations
with real state, Unix sockets and detached PTY workers. Each old CLI's local
update settles only its own installation. After explicit grants and host-book
pins, all twelve directed network connections and four retained-session
reattachments succeed. A real daemon-owned fleet operation then installs the
second authenticated patch on all four hosts while preserving the original
worker and shell PIDs. Only the HTTPS release origin and service-manager
commands are fixture providers. This establishes local ownership and network
recovery; native macOS service management and the installed fleet are separate
live checks. No installed hosts were accessed during this proof.

## Read progress

The preview and status output distinguish the running daemon from retained
session workers. Updating the daemon preserves existing worker processes.
Those workers keep their original code until their sessions end; new sessions
use the new build.

Mesh stages verified artifacts before authorizing activation. It verifies one
canary before authorizing more hosts, with at most two unresolved activation
grants in an operation. A failure stops new grants. Already issued grants can
finish, including after cancellation. Cancellation prevents pending work from
being authorized later.

An activation helper stops the old daemon before replacing the executable. It
checks the new daemon's actual executable, database version, and retained
worker processes. A failed activation restores the previous executable and
checks it against the current database. It does not restore an old database
snapshot. New worker creation is briefly gated during validation; existing
workers continue running.

Each update keeps the replaced executable beside the installed one so a failed
activation can restore it. The latest update's copy stays after it succeeds.
Copies from earlier updates are removed when the next update commits or starts.
A copy stays while a running session still uses it, because macOS identifies
that session's executable through the copy.

If the machine itself reboots during installation, the helper resumes its
durable transaction and waits for the daemon to start. Processes from the prior
boot cannot survive. The result lists those sessions as interrupted and keeps
their recovery records; it does not describe them as preserved live sessions.

Exit status `0` means the selected operation completed, `1` means failure or
blocked compatibility, and `2` means work remains pending. `--json` produces
structured output suitable for scripts. A cancelled operation may still have
issued work to observe with `mesh update status RUN`.

## Update reminders

The picker and compact window show cached release notices. Press `u` to review
the update, `d` to postpone its reminder for 24 hours, or `v` to skip that
release's reminder. Dismissal does not hide unfinished operations.

Checks run in the background with a five-second deadline and a six-hour cache
with jitter. Interactive attachment can start a detached check when no daemon
is running. Notices stay out of session output, shell hooks, and JSON output.

## Storage and compatibility

Set `MESH_UPDATE_CACHE_DIR` to choose the artifact cache. Set
`MESH_UPDATE_REQUIRED_MOUNT` to require a particular mounted data volume before
downloads or activation. For example:

```bash
MESH_UPDATE_CACHE_DIR=/work/cache/mesh-updates \
MESH_UPDATE_REQUIRED_MOUNT=/work mesh update --local
```

Set these variables in a coordinator or target daemon's service environment
when that daemon owns the update. The helper retains the approved installation
settings across restarts. Pending operations keep their pinned artifacts.
Finished fleet operations release their old download directories. Mesh keeps
the latest installation's download and every unfinished operation's artifacts.
Cache cleanup waits until an installer finishes copying a downloaded image.
The helper keeps its current image, pending activation, installation source,
the images named before the latest promotion, and one spare image for recovery. Obsolete helper copies are removed after a
successful helper preparation or upgrade.

An update requires an official release manifest with verified artifact hashes,
compatible state and worker protocols, and a tested transition from the actual
installed executable. A version label alone is insufficient. Unsupported or
package-managed installations are reported for explicit handling. Mesh does
not downgrade a newer installation to complete an older operation.

Each release tests transitions from the previous eight stable releases when
their platform artifacts exist. The immediately previous release is required.
An older or custom build needs a matching tested transition in the manifest;
it cannot bypass this check by using the same version label.

## Release delivery

A main-branch push runs the complete CI gate before entering the serialized
publisher. Publication reserves an exact source commit and version, builds the
final artifacts, runs native transition checks for each supported platform,
and publishes only after the manifest and checksums are complete. An older
queued commit cannot replace a newer published release. A retry retains its
reserved commit and version. If an earlier candidate failed publication, a new
source commit receives the next unreserved version; it never moves the failed
candidate's tag. Updating the Homebrew cask does not start another release.
The Cask PR uses normal pull-request CI and protected auto-merge. If GitHub
requests workflow approval, a maintainer with write access selects **Approve
workflows to run** on that PR. The finalizer does not dispatch a second CI run.
A draft Cask PR must be marked ready for review in GitHub before finalization
can hand it off.

For a custom installed build, a maintainer can attach its executable to the
reserved draft before publication starts. Name it
`baseline_OS_ARCH_SHA256.bin`, using `linux_amd64`, `linux_arm64`, or
`darwin_arm64` and the executable's lowercase SHA256. Reservation freezes that
asset list. The native platform job verifies the hash and exercises the same
forward upgrade, retained-session, saved-recovery, and candidate restart checks
as published baselines. Failed proofs block publication. Receipts certify the
candidate opening retained state and restarting with preserved sessions and
recovery records. They provide no guarantee that a retained executable can reopen
a newer schema. A published manifest is not extended afterward.

Publishing makes an update available for review. Fleet activation follows the
approval recorded by `mesh update`; an available release does not silently
restart every machine.

## Enable automatic updates for a saved fleet

Automatic updates use the coordinator's own `fleet.json`. Device access, update
administrator trust and fleet notes do not enable them. The coordinator must be a
member and already have update-administrator admission on every recipient.
The existing update helper must already be installed on every recipient.

Inspect the coordinator before enabling it:

```bash
mesh update automatic status --coordinator COORDINATOR --json
mesh update automatic enable --coordinator COORDINATOR --expected-revision 0 --yes
```

The approval prints the saved fleet, membership digest and pending enrollment
epoch. On each recipient, including the coordinator, authorize that exact fleet
and epoch locally. Use its current consent revision, zero for a new binding:

```bash
mesh update automatic authorize COORDINATOR_ID --fleet FILE --epoch E --expected-revision 0
```

This command does not install a helper, create an identity, grant device access or
start a daemon. It cannot use `--coordinator`. Replace a local binding only after
revoking it and settling its outstanding installation. The policy waits for all
initial recipient confirmations. Each subsequent automatic stage, retry and grant
checks the recipient's current consent revision again.

The existing release checker saves the complete checked manifest. The coordinator
consumes that observation without another feed request, pins one release and
fleet, and uses the existing installation engine. It chooses one actual
installation as canary. An already-current machine or a joined manual
installation does not unlock later activation grants. A failed direct transition
requires manual bridge review. Automatic updates do not discover an intermediate
release chain.

## Stop and recover automatic work

Inspect status for the current administrative revision before each change:

```bash
mesh update automatic disable --coordinator COORDINATOR --expected-revision N
mesh update automatic revoke --expected-revision N
mesh update retry RUN --coordinator COORDINATOR --expected-policy-revision N
mesh update cancel RUN --coordinator COORDINATOR --expected-policy-revision N
mesh update automatic acknowledge-stop RUN_OR_POLICY_ID --coordinator COORDINATOR --expected-revision N
```

Disable stops new dispatch intentions and supersedes an explicit continuation.
Previously issued activation work may still finish. Target-local revoke fences
new recipient acceptance, including delayed requests and identical replays.
Neither operation retracts a grant the target already accepted.

Failure and cancellation are durable stops. Repeated checks, a newer release,
restart, ordinary enable and plain manual approval cannot clear them. Explicit
retry approves only the same pinned run and one new attempt. If the policy is
disabled, status shows its durable continuation authority. A later disable or
cancel supersedes that authority. Retry response loss does not allocate another
attempt. Supply the same `--command-id` and original expected revision to replay
the same administrative decision.

Cancellation disables the policy. After all issued and externally owned joined
work settles, `acknowledge-stop` archives the run as status-only history and leaves
the policy disabled. A separate enable is required for future releases. A local
coordinator may use `enable --take-ownership` only after this disabled, settled and
acknowledged state. That decision advances the epoch and needs fresh recipient
consent.

Automatic status includes policy ownership and revision, exact membership,
enrollment observations, pinned target progress, canary, sticky stop, explicit
continuation and last completed fleet release. A corrupt or unsafe authority file
has an unknown revision, not revision zero. A local fallback names coordinator
transport failure separately from saved state. Saved intent is not proof that the
coordinator is running. Configuration delivery and dashboard drift presentation
are separate work and are not enabled by binary enrollment.

A stop before the first run has no run to cancel. Explicitly disable the policy,
then pass its exact policy ID to `acknowledge-stop`. This is allowed only when no
current run, executable continuation or unsettled issued work exists. Its receipt
retains the stop cause and approving actor. Background polling cannot recreate an
acknowledged disabled stop. If a current run exists, use its run ID and complete
the cancellation and settlement path first.

## Deliver approved configuration separately

`mesh update config` delivers an explicitly approved usage-feed URL through a
separate target-local grant. It does not require automatic binary enrollment or
restart processes. Preview binds the actual approver and exact selected values.
Status distinguishes saved bytes from a running dashboard's acknowledged load
and feed result. See [Deliver dashboard feed settings](fleet-configuration.md)
for allowance, preview, approval, cancellation, revocation, and recovery.

Domain and feed delivery have separate grants and component receipts. See [fleet configuration](fleet-configuration.md) for saved versus actual loaded policy and restart requirements.

## Inspect fleet drift

`mesh update status` includes independently enrolled automatic and configuration
status alongside saved operations. `mesh update automatic status` shows the
binary policy alone. Default output names the affected machine and next action.
Use `--details` for identifiers and stored diagnostics, or `--json` for the
complete operational values without a human preamble. Privacy mode hides human
details; structured output keeps the original identifiers.

When saved enrollment is present, the no-run JSON status is an object with
`runs` and `updates`. Without enrollment it retains the ordinary run array.
A named run keeps its existing JSON shape. Each update source carries its pinned
coordinator, independent status availability, observation time, and complete
binary or configuration value.

The dashboard and TV show a bounded update attention area. Binary status keeps
the pinned target release, last observed daemon version, last successful fleet
release, offline members, canary, and sticky stop separate. An offline member
still belongs to its original pinned release. An unavailable or refused status
read does not mark the machine unreachable. A direct-transition block retains
manual bridge-review advice; Mesh does not step through historical releases.

The CLI owns the dashboard's joined read-only watcher. It uses existing local
policy, recipient consent, or configuration grants and the exact saved fleet
endpoint. Binary and configuration coordinators may differ. No relevant saved
enrollment means no automatic polling. Each status request has a five-second
deadline and the watcher polls every 30 seconds. It does not fetch release feeds,
wake machines, enroll recipients, create identities, or change trust. Closing
the dashboard cancels and joins its requests.
