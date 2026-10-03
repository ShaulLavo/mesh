# Update Mesh

Run `mesh update` to review and update the machines in your saved fleet. Mesh
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

The first interactive update shows this machine and the hosts in its local
address book. Confirm that this is your complete fleet before saving it. If a
machine is missing, include it in a fleet file before approving the update.
An address book on one machine does not establish the membership of your mesh.

## Choose the machines

```bash
mesh update --all                 # the saved fleet; also the default
mesh update --local               # only this installation
mesh update --host server --host laptop
mesh update --fleet ./fleet.json
mesh update --version v0.2.0       # pin a published version explicitly
mesh update --fleet ./fleet.json --yes --json
```

`--yes` requires a saved fleet, an explicit fleet file, or a narrower selection.
It does not create a fleet from whatever happens to be reachable. `--check`
prints the proposed scope and available release without installing it.

The default fleet file is `fleet.json` beside `hosts.json`. `MESH_CONFIG_DIR`
changes this directory. A fleet lists stable Mesh public identities and explicit
connection addresses:

```json
{
  "version": 1,
  "name": "personal",
  "revision": 1,
  "members": [
    {
      "id": "MESH_HOST_PUBLIC_ID",
      "alias": "server",
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

Use `--coordinator ALIAS` when starting or inspecting an operation on another
adopted host. Starting a fleet operation without a local daemon includes its
one-time setup in the approval preview. The independent helper starts the
coordinator and resumes the saved operation if the command closes during setup.
With `--local`, approval installs only the update helper without adding a hosting
daemon.

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
Edit the destination's local administrator policy when withdrawing that authority
too.

## Cross the control-authentication cutover

The minimum signed-updater bridge release is **v0.1.39** (the first release
containing `8b14666`). Prefer the known pre-cutover baseline **v0.1.147** or a newer
pre-cutover release, so later updater and retained-worker fixes are included. A target older than that minimum
must receive a bridge through an older coordinator or authenticated system SSH
before the coordinator adopts authenticated network controls.

The TLS peer is an Ed25519 key, verified by possession during TLS 1.3 and matched
to an owner-controlled pin or grant. Certificate names, public CAs and certificate
dates do not grant or remove Mesh authority. An encrypted acceptance byte lets
the client observe the server's grant decision before a control dial succeeds.
Handshake admission has a five-second deadline and a 64 KiB inbound budget;
reconnections perform a fresh handshake with session tickets disabled.

Grant mutation takes an exclusive lock and frame admission reads under a shared
lock. Later frames are denied once revocation publishes; already admitted work
may finish. A 250 ms current-grant check also closes idle control and SSH
connections. This retirement is polling-based, so scheduler or storage delays can
postpone socket closure; frame admission still checks the current grant. Revoking
a key does not signal its retained worker processes.

The upgraded coordinator permits one mixed-version exception: its signed update
client may reconnect with the older WebSocket framing when the destination does
not negotiate `mesh-control-tls-v1`. Both challenge and action are signed, bound
to the pinned target and actor, and use a single-use expiring challenge. Replies
must carry the pinned destination's signature. This path sends only
`update.control`; it cannot create a session or bootstrap a pre-updater daemon.
Failed TLS authentication never enters this bridge. A malicious downgrade can
remove stream confidentiality for that signed update exchange; it cannot mint a
control or turn an update administrator into a session controller.

For the omarchy/macbook-air/pi/vps fleet, execute this order through the approved
root coordinator and trusted account-local enrollment paths:

1. Inventory each installed binary, destination account, stable Mesh key, saved
   destination pin and update-administrator grant. Keep authenticated system SSH
   available. The VPS daemon's account is root; every full-device approval there
   requires `--allow-root`.
2. While the old coordinator can still bootstrap pre-updater hosts, bring every
   such target through v0.1.39 or later, preferably v0.1.147. Enroll the root
   coordinator's existing key in each target's update-administrator policy.
3. Approve each actual controlling device's key on its destinations, including
   daemon-to-daemon publishers, certificate distributors and wake witnesses.
   Record witness destination pins in each controlling account's host book.
   Discovery alone cannot enroll a witness. Do not copy a root account's key to
   a user account or assume they share Mesh state.
4. Install the authenticated client binary in the coordinator's account while
   retaining its old daemon until the remote targets settle. Update macbook-air
   and pi, then the VPS; honor explicit fleet dependency ordering where it changes
   that sequence. The new client's signed updater can manage their old framing.
   Ordinary session clients need the authenticated binary before connecting to a
   hardened destination.
5. Update the coordinator daemon (omarchy unless the approved fleet selects a
   different coordinator) last. Retained workers keep their original binary and
   PID; new sessions use the new build. Confirm existing-session reattachment
   from a still-approved device before withdrawing temporary grants.

This order is covered by disposable local fixtures. The installed fleet, root
VPS, Pi and gateway were not accessed or updated during the implementation lane.
If a pin changes or a host lacks its grant, repair it through authenticated system
SSH or a destination-local command; do not reopen unauthenticated controls.

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
