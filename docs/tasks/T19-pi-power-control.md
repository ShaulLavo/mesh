# T19: Wake and sleep inhibition

Status: complete. Verified 2026-09-05.

## Contract

```bash
mesh wake allow      # on the target, allow Mesh machines to wake it
mesh wake pc         # choose a sender on the PC's LAN, wait for the PC
mesh pc              # a failed explicit connection may wake the PC
mesh wake deny       # on the target, disable its permission
```

Permission belongs to the target. Mesh selects the sender from the current
machine and awake Tailnet peers. A Pi is useful because it stays awake, but it
has no special role. Terminal traffic still goes directly to the target.
See [Wake a machine](../power.md) for setup and hardware checks.

The user settled D25 and D26 on 2026-09-05. These choices replace the original
fixed `Via` configuration and the contradictory ban on protocol changes.

## Architecture and decisions

The existing `WakeFunc`, picker wake action, and `edge.Waker` were callers;
remote wake required new protocol messages. Wrapping the general host dialer
would also wake hosts during `mesh ls`, so initial wake belongs to explicit
create, resume, and attach paths.

Two designs were compared: target-signed transferable permission and a registry
of permissions learned directly from every target. Signed permission was chosen.
A sender can help without first adopting the PC or seeing it awake. The registry
would require continuous discovery before the first wake could work.

Each signed grant occupies a separate cache file. `hosts.json` stays unchanged;
no configuration migration, new Go dependency, or database table is needed.
Adoption and verified host-info reads cache permission. Daemons distribute their
own current grant every minute and after local permission changes.

The modules own these responsibilities:

- `internal/wake`: target policy, signatures, bounded caches, NIC and gateway
  discovery, LAN matching, witness probes, UDP, and durable sender cooldown.
- `internal/wakeclient`: sender selection, independent observation, concurrent
  wake attempts, request correlation, and waiting for the pinned target.
- `internal/protocol`: optional `HostInfo.Wake` and `wake.configure`,
  `wake.remember`, `wake.probe`, and `wake.send`, plus their response types.
- `internal/daemon/wake.go`: local-only permission changes, bounded handlers,
  background grant distribution, and the public-edge adapter.
- `internal/cli/wake.go`: commands and explicit connection intent.
- `internal/transport`: conservative recovery and input generation boundaries.
- `internal/inhibit`: one child lease while the daemon observes live workers.

Phase checklist:

- [x] Trace callers, daemon dispatch, reconnect, and edge publication.
- [x] Compare two designs and choose the permission lifecycle.
- [x] Record the user's decisions and implement the selected interfaces.
- [x] Replace nil wake dependencies and protect input across reconnects.
- [x] Finish repository verification and review the actual artifact.

## Permission and LAN identity

The target signs its identity, permission, revision, validity interval, and NIC
metadata with its existing Ed25519 key. Only the target's Unix daemon socket
accepts permission changes. The direct listener retains D23's Tailnet ACL
admission model.

Grants expire after 30 days and renew while the target is awake. A disabled
record has a higher revision; senders that have seen it reject older grants.
Offline peers can miss revocation until reconnection or expiry. Deleting the
policy resets its revision history and is not a supported permission reset.

Sender selection requires both the IPv4 subnet and gateway MAC to match.
Private subnets alone cannot distinguish two homes using `192.168.1.0/24`.
Targets use their wired default-route NIC; senders may use Wi-Fi. Missing route,
MAC, or gateway data makes a sender unavailable. Failed discovery does not renew
an old grant's expiry.

The sender derives the broadcast address from the signed prefix. One dispatch
sends the 102-byte magic packet to UDP ports 7 and 9. A persisted 90-second
cooldown deduplicates concurrent processes on the same sender. Clients share
concurrent attempts within a process and do not switch senders after a lost send
acknowledgment. Independent senders do not provide globally exactly-once delivery.

## Wake intent and recovery

Explicit host connection, resume, session attach, and `mesh wake` can wake.
Catalog and picker refresh calls never send wake packets. Success requires the
expected target identity to answer within 90 seconds. A transmitted
session-create request is never retried by wake logic.

Dropped attachments require an independent remote sender on the same LAN.
Its ICMP check must find the target unreachable while the gateway responds.
Missing tools, an unreachable witness, or inconclusive checks leave normal
reconnect backoff running. This establishes reachability, not a hardware sleep
state; catalogs report offline rather than claiming the machine is asleep.
An inconclusive recovery can retry after 30 seconds; a successful recovery ends
wake attempts for that connection outage.

Buffered input cannot cross connection generations. Input on a replacement
connection waits for the session's attach acknowledgment. Cold-booted targets
still report old sessions as interrupted. Failed resume removes transport
tracking and never creates a replacement shell.

The public edge uses the same wake client and caches permission while pinning an
origin. `--wake-on-request` waits up to 90 seconds. The existing fresh-publication
check still gates proxy dispatch after wake.
A TCP check detects a sleeping origin even while its last heartbeat remains
fresh. It reads no HTTP body and never retries an ambiguous HTTP request.
Wake waits use a separate 32-slot limit so healthy origins retain upstream slots.

## Sleep inhibition

The catalog drives one inhibitor for running or detached workers. Startup
reconciliation includes preserved workers; the last exit releases the inhibitor.
Inconclusive worker probes retain the existing active observation.

Linux uses `systemd-inhibit --what=sleep:idle --mode=block --no-ask-password`;
macOS uses `caffeinate -i`. Both wrap `/bin/cat` reading a pipe whose write end
belongs only to the daemon. SIGKILL closes the writer and ends the lease.
Cleanup is bounded, children are reaped, and unavailable mechanisms report once
without preventing sessions from running.

## Verification

`scripts/check-t19.sh` retains focused race tests, vet, real daemon and WebSocket
scenarios, child lifetime checks, and Linux amd64/arm64 plus Darwin arm64 builds.
`scripts/verify.sh` includes the power-control integration scenario.

Tests cover signed permission, expiry, overlapping subnets, durable revocation,
concurrent sender cooldown, exact UDP bytes, unknown witnesses, pending input,
catalog reads without wake, and daemon death. Physical suspend/resume, firmware
Wake-on-LAN, macOS power assertions, and requests through the deployed VPS remain
operator checks on the actual machines.

The full race suite, vet, all 28 integration scripts, module/format checks, and
the three platform builds passed. Full lint retains eleven existing findings
in unchanged bootstrap, installer, and tag files; no new findings remain.
