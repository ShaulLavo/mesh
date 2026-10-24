# T27 — Hibernate idle agent sessions

**Status:** implemented · **Prerequisites:** T24, T25

## Outcome

A detached session whose Codex or Claude agent has sat idle stops holding
memory, and attaching to it reopens the same conversation. `mesh ls` shows what
each session costs and how long it has been quiet, and `mesh gc` reclaims idle
sessions in one command. See [the user guide](../hibernation.md).

On the host that prompted this, sixteen detached sessions left for three to
thirteen days held about 3.5 GB, almost all of it in idle agent processes, and
kept the machine from sleeping.

## Design

Hibernation is T25 recovery triggered on purpose. The worker stops the session
the way `mesh kill` does, but keeps the agent recipe `active` instead of letting
the launch helper's exit mark it `closed`. T24's default recovery action already
resumes an ended session with an active recipe, so every existing path wakes it:
`mesh recover`, the picker's Enter, SSH recovery and older clients.

- The worker owns the decision. It refuses an attached session and one without
  a registered, active invocation, and for idle requests re-checks its own
  detach time and output clock. It writes `hibernation.json` durably before the
  stop, turns away attachments while stopping with reason `hibernating`, and
  skips the first-attach wait so a never-viewed session stops promptly.
- Workers record `detachedAt` when the last client leaves. `lastAttachedAt`
  marks when an attachment began and cannot measure idleness.
- The daemon's `--hibernate-idle` loop reads only persisted state, scans about
  four times per idle period, forwards `session.hibernate` with the threshold,
  and logs each refusal once. Zero, the default, disables it: the stop is a
  policy the host owner opts into, not a change to invariant 2.
- A hibernated session stays `exited` on the wire and carries `hibernated`, so
  clients that reject unknown states still list the host. A source already woken
  into a replacement no longer reports it.
- Lists carry `memoryBytes`: Linux PSS plus SwapPss of the worker's process tree,
  macOS resident size, sampled at most every ten seconds.

Automatic capture needed one more change. T25 accepted only the exact probed
provider versions, which switched capture off for every real installation within
days of each provider release. The check now accepts the probed version and later
releases in its major line; resume still counts as verified only when the
provider's hook confirms the saved ID.

## Delivery notes

`integration/hibernation.sh` drives a real daemon and worker with a fake Claude:
an idle detached agent hibernates with its recipe active while an equally idle
plain shell keeps running and refuses explicit hibernation; default recovery
reopens the exact conversation with `--resume=ID` and a verified receipt; a
resumed conversation hibernates again on request and wakes again.
`integration/hibernation_cli.sh` covers `mesh ls` columns, `mesh hibernate`,
`mesh gc` plans and actions, and wake on `mesh ID`.

Limits: a session created through the daemon and never attached records no
detach time, so the idle policy never selects it; `mesh hibernate` still can.
Workers started by an older binary record no detach time either and stay
running until they are replaced. The SQLite catalog cache does not store the
new fields, so a stale row from an offline host shows `exited`.
