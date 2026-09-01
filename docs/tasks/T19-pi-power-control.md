# T19 — Pi power control

**Status:** not started · **Blocked by:** T06, T07, T08, T09 · **Owns:**
`internal/wake/`, `internal/inhibit/`, `internal/cli/wake.go`,
`cmd/mesh/bootstrap.go`

## Goal

```bash
mesh add shaul@pc    # records the NIC MAC while the machine is still reachable
mesh wake pc         # the Pi sends the packet; returns when pc answers
m pc                 # an asleep host wakes itself, then attaches
```

Step 6 of the build order, and the only numbered step that never got a brief.
Every layer already has the seam. Nothing fills it.

## What already exists

This task writes an implementation, not a design. The integration points were
built by the tasks that needed them and are already tested against fakes:

- `cli.WakeFunc` (`command.go:49`), `mesh wake host` (`command.go:482`), and the
  picker's wake selection (`tui/picker.go:81`) all reach a nil dependency and
  return `host %s has no wake controller configured`.
- `edge.Waker` (`edge/proxy.go:58`) with `WakeTimeout` and `wakerConfigured`.
  The only implementation is `noWaker`, which returns `wake is not configured`.
- `WakeOnRequest` rides three protocol control messages, `mesh serve
  --wake-on-request`, and the `services.wake_on_request` column.
- `reservedAliases` already claims `wake`, so no host can shadow the command.

**No protocol message and no table changes in this task.** If you find yourself
editing `internal/protocol`, stop: the seam you need is already there.

## Responsibilities

1. **`internal/wake` — the sender.** A magic packet is six `0xFF` bytes followed
   by the target MAC repeated sixteen times, sent as a UDP broadcast to the
   target subnet's broadcast address on ports 7 and 9. This is roughly forty
   lines and takes no dependency. It must run on a host that shares a broadcast
   domain with the target; a magic packet does not survive a router hop.

2. **Record the MAC at adoption.** `mesh add` already opens SSH and records the
   Mesh identity and Tailscale name. Read the MAC of the interface holding the
   default route in the same pass. Adoption is the only moment the target is
   guaranteed reachable — **a host that is already asleep cannot tell you how to
   wake it**, and recovering the address afterwards means the router's lease
   table or a walk to the machine.

3. **`WakeConfig` on `HostRecord`.** Bump `hostConfigVersion` to 2.

   ```go
   type WakeConfig struct {
       MAC  string `json:"mac"`
       Via  string `json:"via"`
       Auto bool   `json:"auto,omitempty"`
   }
   ```

   `Via` is the alias of the adopted host that sends the packet. Naming the
   sender per host rather than assuming one Pi means a second site works with no
   code change, and it makes the broadcast-domain requirement explicit in
   configuration instead of implicit in deployment.

4. **Wire `WakeFunc` in `cmd/mesh/bootstrap.go`.** The client asks `Via` to send
   the packet over the daemon connection it already has, then waits. Return only
   when the target accepts a connection or the context ends — the exact contract
   `edge.Waker` documents. Wire the same implementation into `edge.HandlerConfig`
   so `--wake-on-request` stops being decorative.

5. **The liveness witness.** `Via` answers whether the target is up, observed
   from the target's own LAN. This is the discovery half of invariant 4 and the
   thing that makes autowake safe.

6. **Autowake inside the existing reconnect loop.** `transport.go` already
   reconnects with `Backoff{100ms, 5s, jitter 0.2}` and replays per-session
   `resumeState` through `sendResumes`. Autowake is a branch in that loop, not a
   new subsystem: after repeated failures, ask the witness, and wake only if the
   witness says the host is down.

7. **The sleep inhibitor.** A host must not suspend while any worker owns a live
   PTY. Losing a session to the host's own idle timer breaks invariant 2 as
   surely as a daemon crash. The daemon holds an inhibitor while any worker is
   running and releases it when the last one exits.

## The part that needs care

**Unreachable is not asleep.** Your wifi dropped, you changed networks,
Tailscale is renegotiating. None of those should physically power on a desktop,
and the client cannot tell them apart alone. Ask the witness first. If the
client cannot reach the witness either, the client's own network is the problem:
keep backing off and wake nothing. This rule is the whole reason autowake is
safe to enable.

**Wake on intent, never on polling.** `m pc`, `m pc -r`, and `m 7K3D` may wake,
because the user asked for a session. `m ls` must never wake. A survey command
that powers on machines produces a host that idles, suspends, gets woken by the
next poll, and never stays asleep — with nothing in any log looking wrong. `m
ls` reports the host as asleep from the witness's answer, which is strictly more
information than it shows today.

**A cold wake destroys the session; discard buffered input.** Two cases look
identical to the client and are not. A host resumed from suspend still owns its
workers, and `sendResumes` replays from the last acknowledged sequence. A host
that cold-booted marks its sessions `interrupted` per invariant 5, and there is
no PTY to replay into. Queued keystrokes must be **discarded on a cold wake, not
delivered**. Replaying input into a freshly booted shell runs half a command the
user typed twenty minutes ago. The retry unit is the intent to attach, never the
bytes.

**One in-flight wake per host.** A cold boot takes tens of seconds and the
backoff loop will fire many times inside it. Send one packet, hold one deadline
covering the worst case, and let later rounds wait on the same result. Reuse the
`edge.Waker` contract rather than growing a second, subtly different one.

**Nothing wakes a machine in G3.** Mechanical off — mains lost, PSU switched,
unplugged — leaves no standby power for the NIC to listen with. This is operator
configuration, not code: the target needs Wake-on-LAN enabled in firmware, ErP
disabled so standby power reaches the NIC, and `Restore on AC Power Loss` set if
it should survive an outage. On Linux the wake bit does not persist across
reboots and belongs in a systemd `.link` file. Document this in the task's
operator notes; do not attempt to configure a remote machine's firmware.

## Open decisions

Resolve these before implementing, and record them in `01-decisions.md`.

- **D25 — how the inhibitor is held.** `systemd-inhibit` as a child process is
  trivial and dies with the daemon, which is the correct failure direction, but
  it costs a process per daemon and assumes systemd. A logind D-Bus inhibitor
  lock is cleaner and holds an fd directly, but adds a dependency and still
  assumes logind. Neither works on a host with no logind at all, which needs a
  documented no-op rather than a startup failure. macOS is a third case
  (`caffeinate`), and the Pi never needs one.
- **D26 — how the witness is asked.** A new control message is explicit and
  independently testable. Riding the existing concurrent host catalog is free,
  since `m ls` already queries every known host and distinguishes live from
  cached — the answer may already be sitting in that result.

## Acceptance

- `mesh wake pc` sends exactly one magic packet from `Via`, and returns only
  once `pc` accepts a connection or the deadline expires. It reports which host
  sent the packet.
- `mesh add` records a MAC. Adopting a host whose MAC cannot be read succeeds
  and leaves wake unconfigured, with a message saying so rather than failing.
- A `hostConfigVersion` 1 address book loads, and every host in it reports wake
  as unconfigured instead of erroring.
- `mesh wake` on a host with no `WakeConfig` fails with a message naming the
  missing configuration, not a nil dependency.
- Suspend a host with a live session, then `m 7K3D`. The witness reports it
  down, one wake fires, the session resumes through `sendResumes`, and no output
  is lost.
- Cold-boot a host with a previously live session, then `m 7K3D`. Sessions come
  back `interrupted`, buffered input is discarded, and nothing is written to the
  new shell.
- Kill the client's network entirely, so neither the target nor the witness is
  reachable. No wake is attempted, and the backoff loop keeps its existing
  behaviour.
- Drive the backoff loop through many failed rounds during a slow wake. Exactly
  one packet is sent.
- `m ls` against a suspended host reports it asleep and sends no packet. Run it
  in a loop for longer than an idle timeout and the host stays asleep.
- Start a session on a host with an idle-suspend timer shorter than the session.
  The host does not suspend. The inhibitor is released when the last worker
  exits, and a host with no inhibitor mechanism logs once and continues.
- Kill the daemon while it holds an inhibitor. The inhibitor is gone.
- `mesh serve --wake-on-request` reaches a real waker: a public request for a
  sleeping origin wakes it within `WakeTimeout` and is served, and a request
  that exceeds it fails cleanly rather than hanging.
- Terminal traffic still goes host to host. The witness answers questions and
  sends packets; it never carries a session.

## Out of scope

Waking over wireless (WoWLAN), which is separately supported and unreliable.
Relaying a wake through a chain of hosts to reach another segment: a target
without a waker on its own broadcast domain is unconfigurable, and says so.
Scheduled or calendar wake. Suspending a host remotely — `mesh sleep` is a
plausible later task and a bad idea to combine with autowake in this one.
Configuring a remote machine's firmware or NIC wake bit.
