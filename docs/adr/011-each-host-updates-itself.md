# 011. Each host updates itself

Status: accepted
Date: 2026-11-20

## Context

Mesh first kept a fleet current through a coordinator policy. One machine held
an approved fleet, every recipient signed a local consent bound to that exact
membership and epoch, and the coordinator pinned one release for the whole
fleet. It could not catch up a machine that had missed several releases, it
voided every consent when a machine joined, a member that stayed offline held
back every newer release, and each failure was a stop a person had to clear.
None of the fleet's machines was ever enrolled in it.

## Decision

The daemon on each machine runs a host updater that plans and installs releases
for that machine with its own authority. It verifies signed release metadata,
walks a chain of tested transitions from its own build, and rolls back on its
own when a hop fails. Fleet order comes from evidence: a host reads what its
configured predecessors actually committed and installs the newest release they
have all run for the soak period. `mesh update policy` sets the mode,
predecessors and soak on each machine, and `mesh update fleet` reads every
machine's state.

The coordinator policy, its recipient consent, and the `mesh update automatic`
commands are removed. Manual `mesh update` fleet operations, update
administrator trust and configuration delivery stay.

## Alternatives considered

- **Keep both mechanisms.** The policy and the host updater compete for the
  same installation journal, so the host updater had to stay idle wherever a
  consent existed. Two mechanisms meant two failure models and two status views.
- **Extend the coordinator policy.** Chain planning, offline catch-up and
  membership changes would all have to run through one machine, which
  contradicts each host being authoritative for itself.

## Consequences

No machine needs a coordinator, a saved fleet or an enrollment to stay current.
An unavailable predecessor holds the hosts that follow it and is reported in
`mesh update fleet`; nothing expires into permission. A person who wants a
specific fleet-wide release still runs a manual `mesh update`, which can join an
installation the host updater already started.
