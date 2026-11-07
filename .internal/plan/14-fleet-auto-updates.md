# Keep the fleet current

Status: Approved, 2026-10-09. Execution follow-up to
[T26: fleet updates](../tasks/T26-mesh-updates.md). T26's requested operations,
release reminders and installation helper are implemented. Automatic binary policy source is implemented in Approved audit Plan 017 unit 2,
subject to independent review, current checks and merge.
Feed-only configuration delivery is implemented in Approved audit Plan 017 unit 3,
subject to independent source review, current checks and merge. Domain delivery,
dashboard fleet drift presentation and actual fleet enrollment remain separate
later units and maintenance. Source acceptance does not enroll
the owner's fleet.

## Outcome

Enroll a saved fleet once. Its coordinator then installs new tested stable
releases automatically, finishes machines that return later, and reports any
machine that remains behind. A deployment change includes the enrolled
consumers whose configuration must change with it.

The October 9 incident exposed both gaps. The PC had moved to a newer build
and the new deployment domain while the Pi remained on v0.1.196 from October 4.
The Pi's reminder detected v0.1.226 on October 9, but its update history had no
newer installation request. Its domain setting and dashboard feed URL still
used the retired namespace. Updating the executable alone left the domain
setting unchanged and continued to report the PC as unreachable.

## Execute in this order

1. Add a persistent automatic-update policy to an explicitly saved fleet.
   Enrollment records its exact membership and the coordinator authorized on
   every member. Enable, inspect and disable it through `mesh update` commands.
   Show this policy and the last completed fleet release in update status.
2. Reuse the daemon's existing bounded release checks to start one durable
   operation for a newer stable release. Pin that release and membership when
   the operation starts. Repeated checks and coordinator restarts resume it.
   Keep one operation active at a time; inspect the newest release after it
   finishes. Disabling the policy stops new operations and retains issued work.
3. Block automatic work when an official direct transition is unavailable. Keep
   the pinned release and explicit manual bridge-review guidance. Approved audit
   Plan 017 excludes unattended intermediate release chains and bootstrap paths.
4. Make deployment changes deliver their approved domain and consumer settings
   to every enrolled affected host. Keep this configuration delivery explicit
   and separate from release installation. Preserve unrelated settings and
   credentials. Replace invalidated remote catalog caches, reload the daemon
   and dashboard, and verify the actual service and feed from each consumer.
   Use [the deployment-domain plan](12-deployment-domain.md) for namespace rules.
5. Show fleet drift on the dashboard and in `mesh update status`: intended
   release, last successful update, pending/offline machines and the next action
   for a blocked machine. Keep a reachable host with an invalid configuration
   distinct from a network failure, with its diagnostic available in details.
6. Ship and enroll the owner's saved PC, Pi, Mac and VPS fleet. Run a real
   automatic operation and verify each running daemon, the Pi's live host
   readings, service URLs and AI feed. Record the completed release and policy
   in the delivery evidence before marking this plan complete.

## Verification

- A newly published stable release starts exactly one operation under the
  enrolled policy. A fleet without that policy retains requested updates.
- Closing the initiating terminal and restarting the coordinator during staging
  or activation preserve the chosen release and resume unfinished work.
- An offline member catches up when it returns. A newer release published while
  it is offline cannot replace the release pinned to the active operation.
- An unsupported direct transition stops automatic issuance and names manual
  bridge review. No historical chain or unattended bootstrap is attempted.
- A failed canary stops further grants and remains visible. A subsequent check
  cannot start another operation around that failure.
- Domain delivery preserves unrelated configuration. After the change, all
  affected consumers read current host identities, service URLs and feed data.
- The TV shows a reachable host with a configuration error separately from an
  unreachable host. Unknown quota remains unknown throughout feed interruptions.

## Incident evidence and reproduction

On the Pi, run `mesh update status --json`, `mesh version`, `mesh ls` and
`mesh serve ls`. Compare the installation journal with the release recorded in
`~/.local/state/mesh/update-notice/notice.json`. Check the configured namespace
in `~/.config/mesh/domains.json` and the dashboard's `usageFeedURL` in `hosts.json`.
Do not print credential files or unrelated configuration.

The live failure was `host pc returned an invalid private name; cached rows are
stale`. The dashboard reported `pc · cached name · unreachable` and `AI plans ·
feed unavailable`. Direct Tailnet connections and the current feed worked.
The successful repair installed v0.1.197, v0.1.205, v0.1.213, v0.1.221 and
v0.1.229 through the existing updater, delivered the current namespace and feed
settings, withdrew eight obsolete cached service rows after a local backup, and
restarted the daemon and TV dashboard. All four hosts and three feed accounts
then appeared on the TV. Source anchors are `internal/cli/update.go`,
`update_bridge.go`, `update_helper.go`, `update_notice.go`, `remote.go` and
`internal/tui/dashboard_usage.go`.


## Feed-only source boundary

Plan 017 unit 3 adds target-local usage-feed grants and signed exact preview,
approval, status, retry and cancellation. Recipient intent and selected-value
CAS use the existing CLI configuration writer. The running dashboard replaces
its joined fetcher and acknowledges its generation through its real renderer.
Observer renewal is independent of the feed fetch interval. No active dashboard
is reported as saved without consumer observation. Cancellation stops new
intentions, while target-local revocation separately fences acceptance.

Domain-policy delivery, catalog generation and actual service verification are
unit 4. Fleet drift presentation is unit 5. No real feed URL, fleet enrollment,
owner configuration, provider account or installed service was changed by unit 3.


## Domain source boundary

Plan 017 unit 4 adds explicit domain grants, component receipts and actual
signed daemon policy observations. The primary and complete aliases retain all
accepted private domains and the legacy certificate owner. Policy generation
serializes domain publication with every advisory service-cache operation and
invalidates retained live service and private-name observations. Process policy
stays immutable. Saved changes remain restart or reopen required until actual
consumer observations match.

Each process retains its first catalog generation across cache and watcher
reconstruction. Returning to the same policy bytes does not admit an older
process into a newer generation. Temporary observation failure keeps retained
rows explicitly stale instead of reporting a policy change. A domain conflict
does not strand an independently prepared feed, and a terminal conflict cannot
resume through replay. Native and SSH one-shot commands infer naming policy
without publishing deployment metadata.

Service HTTPS verification is explicitly unknown pending a request-time passive
admission contract. No service probe, automatic restart, enrollment, installed
update or owner configuration change is part of this source unit. Direct
historical release transitions remain blocked with explicit manual bridge-review
advice; automatic reconciliation does not traverse historical releases.
