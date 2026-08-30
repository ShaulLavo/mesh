# Status

Updated 2026-08-30.

## Done

Steps 1 and 2 of the build order, plus the daemon.

- `internal/protocol` — framing + control messages, shared by every transport
- `internal/session` — byte-offset replay ring, session IDs
- `internal/worker` — PTY ownership, Unix socket, attach/steal, bounded outbound
  queues, resize, signals, kill escalation, `meta.json` lifecycle record
- `internal/terminal` — rendered screen snapshots for clean reattachment (T01)
- `internal/storage` — SQLite session and host store (T03)
- `internal/daemon` — worker discovery, reconciliation, relay, lifecycle (T04)
- `internal/transport` — WebSocket transport with resume (T05)
- `internal/identity`, `internal/tailnet` — host keys, Tailscale discovery (T06)
- `internal/cli`, `cmd/mesh` — Cobra + Fang product surface, versioned host
  address book, concurrent live/cached host catalogs, remote create, attach,
  logs, kill, signals, and the T09 picker boundary (T07)
- `internal/serve` — durable static, files, and loopback-proxy services; hardened
  shared root resolution; live service controls and restart restoration (T11)

Verified 2026-08-30: `gofmt`, `go vet ./...`, `go test -race ./...`, and all eleven
scripts in `integration/` passing.

Origins can now serve on their tailnet HTTP listener, but no DNS or certificate
has been configured for `mesh.shaulavo.dev` and no public edge exists. Step 9 is
entirely unwritten.

## Complete tasks

T01 vt snapshot · T02 outbound queue · T03 storage · T04 daemon · T05 websocket
transport · T06 host identity · T07 CLI surface · T11 serving core.

## Next

```
T07 CLI surface ──→ T09 picker TUI ──────────────→ T17 sessions over SSH
             └───→ T10 packaging                        ↑
T08 ssh bootstrap                                       │
T15 ssh front door ─────────────────────────────────────┤
             ├──────────────────────────→ T16 SFTP and SCP
             └──────────────────────────→ T18 reverse tunnels
                                                   ↑
T11 serving core ──┬─→ T12 private names ──→ T13 public edge ──→ T14 m serve
                   └──────────→ T16                     └────────→ T18
```

T08, T09, T10, T12 and T15 are unblocked. Everything else waits on one of them.

| Task | Owns | Blocked by |
|---|---|---|
| T08 ssh bootstrap | `internal/bootstrap/`, `scripts/install/` | — |
| T15 ssh front door | `internal/sshd/` | T06 |
| T09 picker TUI | `internal/tui/` | — |
| T10 packaging | `.github/`, `.goreleaser.yaml` | — |
| T12 private names | `internal/dnsname/` | — |
| T13 public edge | `internal/edge/` | T11, T12 |
| T14 `m serve` | `internal/cli/` | T11, T12, T13, T07 |
| T16 SFTP and SCP | `internal/sshfs/` | T11, T15 |
| T17 sessions over SSH | `internal/sshd/session.go` | T09, T15 |
| T18 reverse tunnels | `internal/tunnel/` | T13, T15 |

T07 and T14 both own `internal/cli/`. Land T07 first.
T15 is the foundation for T16, T17 and T18. Land it before any of them.

## Known defects

Found in review on 2026-08-30. All six are closed; the entry stays so nobody
rediscovers the tradeoff.

**Closed 2026-08-30.**

1. The daemon accumulated zombie workers. `Process.Release()` freed Go's
   bookkeeping, not the OS parent-child link. Reaped with `cmd.Wait()`.
   Covered by `integration/reaps_exited_workers.sh`.
2. `cli.Attach` leaked a goroutine per call: the SIGWINCH goroutine ranged over
   a channel nobody closed, and `relayInput` read stdin after `Attach` returned.
   This was blocking T17.
3. `relay.go`'s `forward()` held `sendMu` across the client write, so a slow
   client stalled every lane it owned. It has the bounded outbound queue now,
   matching what T02 did in the worker.
4. `mesh kill` printed `killed` up to 5s before it was true. Kill is synchronous
   and acknowledged now, per D6. Covered by `integration/kill_waits.sh`.
5. `protocol.Reader.ReadFrame`'s doc claimed the payload was valid only until
   the next call. It allocates fresh every call, and the doc says so now.
6. A child that stopped reading stdin could block its connection goroutine in a
   PTY write. Worker input now crosses a bounded queue, so detach and control
   frames remain responsive.

Cosmetic, nobody has bothered and nobody should make a task of it: `cli/attach.go`
hand-rolls `indexByte` where `bytes.IndexByte` exists; `cli/sessions.go` builds
paths by string concatenation where `launch.go` uses `filepath.Join`; and
`session/ring.go` shadows the builtin `cap`.

The review's earlier coverage figures predate T07. T07 replaced the command
dispatcher and added command-level coverage for host/session routing, stale
catalogs, picker actions, and remote controls.

## Rules for anyone picking up a task

- Read `CLAUDE.md` and `docs/plan/01-decisions.md` first.
- All integration scripts must still pass. Add one if you add behaviour worth
  protecting.
- Do not change `internal/protocol` without saying so loudly: every component
  shares it. Additive fields are fine, renames are not.
- Update this file and the task brief when you land.
