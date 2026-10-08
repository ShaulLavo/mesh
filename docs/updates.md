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

An exact installed release needs no update operation. A compatible newer
installation is kept. Approval is blocked when a checked installation lacks a
tested direct transition, requires recovery, or has unsupported metadata,
protocols, journal format, or running-session protocols. For a missing direct
transition, a local review searches at most 32 published release entries for one
intermediate release. The search has a ten-second budget and verifies both exact
transitions against their content-addressed rollback receipts. The next hop's
version comes from its published manifest.

Mesh suggests `mesh update --local --version VERSION` only after checking the
existing daemon installation, settled update history, service preservation
settings, helper receipt and launcher, helper journal support, and the helper's
running executable. The helper must already be at least as new as the next hop.
The preview keeps the direct update blocked and does not install intermediates.
Run the suggested command on the named destination to review that single step.
Remote targets, first-updater bootstrap, and client-only installations need a
local review before Mesh can establish bridge readiness. If discovery or
readiness fails, the preview says so and gives one scoped review action.
`--details` and `--json` include the original rejection and discovery evidence.

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
`authorized_keys`. Connections capture that line identity under the admission
lock. Immediate same-key reapproval creates a fresh incarnation: old control
and SSH attachments retire even if no poll sampled the absent membership, and
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
controls and ordinary service URLs remain available. The owner accepts this short
mixed-version interval and updates each host independently through system SSH.

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

For a custom installed build, a maintainer can attach its executable to the
reserved draft before publication starts. Name it
`baseline_OS_ARCH_SHA256.bin`, using `linux_amd64`, `linux_arm64`, or
`darwin_arm64` and the executable's lowercase SHA256. Reservation freezes that
asset list. The native platform job verifies the hash and exercises the same
state, retained-session, and rollback checks as published baselines. Failed
proofs block publication. A published manifest is not extended afterward.

Publishing makes an update available for review. Fleet activation follows the
approval recorded by `mesh update`; an available release does not silently
restart every machine.
