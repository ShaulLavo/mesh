# Host update commands and status

Unit 9 keeps the manual default. Status fields are advisory and never authorize
installation, acknowledge retention, or replace the host-order evidence API.

## Status facts

`Store.HostStatus` reads policy, updater state, peer read state, local target,
and committed history under the existing authority lock. Its last pinned commit
lookup examines at most 64 newest ledger entries. Valid unpinned manual history
remains non-permitting and does not become a pinned commit.

`host-status.json` records a successful authenticated discovery observation.
The executing source must match its verified manifest, and original head bytes
must match the exact discovery pin. Partial cursors supply no observation.
The call stays immediately after `saveCursor`, before the held-at-current return.
Target and reason clocks survive polling and restart; a changed target, changed
reason, or settlement resets the corresponding clock.

The summary distinguishes the authenticated compatible `head` from `latest`.
A future latest epoch remains opaque, with `latest: null`. Currentness requires
the executing image to match the authenticated source or the exact pinned head
commit. A fleet result also requires a managed installation, automatic mode,
and a head observation no older than 40 minutes. Checks run every 30 to 35
minutes. Positive release counts remain unknown because anchor ordinals and
version numbers are not release counts.

Unknown targets, publication times, waiting times, latest releases, counts, and
commits use null fields. Broker, state-view, and dashboard copies own every
nested summary pointer. Clone tests check the actual snapshot objects.

## Commands and boundaries

Policy writes, dependent forget, and local target controls require the signed
owner identity through the actual Unix transport. An enrolled administrator or
even the owner key over a network connection cannot supply that boundary.
Fleet status reads ordinary authenticated host info directly.

Local preview calls `PlanLocalTarget`. It returns verified pins without a
request or generation. Approval submits only its exact selected version when
the binary differs, then reports the real durable request and generation.
`status`, `retry`, and `cancel --local` use the actual saved intent revision.
Existing coordinator `RUN` operations remain separate. The obsolete two-hop
Bridge/history search and unused bridge preflight are removed. Forward receipt
verification remains unchanged.

Fleet status returns 0 for current automatic managed hosts, 2 for legitimate
pending work or a recently observed offline host, and 1 for problems, unknown
status, unsuitable installations, overdue waits, or a day without a reply.
Its timestamp cache uses SQLite `mode=ro` and `query_only`, with no database
creation or migrations. Existing caches retain their ownership.
