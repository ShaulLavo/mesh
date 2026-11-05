# 008. Kept routes live in the route state machine

Status: accepted
Date: 2026-11-05

## Context

An on-demand route counts only connections through its Mesh listeners and tailnet path. A client that uses the upstream port directly is invisible to that count, so a shared service such as a model proxy could be stopped by the idle timer while it was busy, and a direct request could not start it again. A successful stop left no log line, and every start removed the route's earlier ended sessions, so the outage that followed could not be explained afterwards.

## Decision

A route can be kept with `--keep-running`. A kept route starts its command at once, never stops it for idleness, and starts it again after any exit with a delay of 1 second doubling to at most 5 minutes; a run of at least a minute starts the delay over. It is a lifetime inside the existing route state machine, not a second supervisor: the worker still owns the process, discovery still adopts it by label, and unknown or uncertain ownership still blocks every launch.

- One admission test decides every start, whether traffic, the kept supervisor tick or `mesh serve start` asks. Traffic cannot shorten or extend a restart delay; only `mesh serve start` overrides it.
- `mesh serve stop` is recorded by the route, not by the waiting caller, so a cancelled caller cannot abandon it. On a kept route it also holds the route stopped until `mesh serve start`, a change of the route's definition, or a daemon restart. The hold is not persisted. An unchanged Sync or a display-name edit keeps it.
- Shutdown is a fence. Close refuses new launches and kills, waits for the ones already admitted, and holds no lock while waiting. A worker created during shutdown is adopted by the next daemon.
- Every completed start, adoption, stop and unexpected exit is logged with the route, the exact session and a fixed cause, never the command, its output or a peer.
- A start prunes the route's history automatically: up to eleven eligible historical exits stay, plus the current attempt and protected records. The ten newest attempts and newest run of at least a minute are selected. Pruning takes the recovery lock before it retires a row and skips anything recovery, an app pin or an interruption protects. App Forget stays the explicit, destructive operation. Background retention ages ended sessions by when they ended.

## Alternatives considered

- **A never-idle switch alone.** It cannot bring a crashed service back for a client that never goes through Mesh, or after a reboot.
- **Sampling upstream traffic.** Watching sockets is platform-specific, cannot see every kind of use, and still cannot wake a stopped port. A listening port proves readiness, not use.
- **A separate supervisor or systemd unit per service.** It would own the same process twice and need its own platform integration on Linux and macOS.

## Consequences

Kept routes trade cold starts for a process that runs all the time. Direct clients see refused connections while the command starts or restarts and must retry; kept does not mean uninterrupted. A finite route still requires every client to go through Mesh, which Mesh cannot enforce. Eligible route history is bounded, not guaranteed: its seven-day age limit, host-wide 500-record cap or continuous churn can remove the extra long-run record. Interrupted, recovery-protected and owner-pinned records can outlive both limits.
