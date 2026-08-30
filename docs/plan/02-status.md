# Status

Updated 2026-08-30.

## Done

The session and transport core, product CLI, SSH bootstrap, packaging, origin
serving core and service catalog, private DNS/TLS path, and authenticated public
edge are complete.

- `internal/protocol` — framing + control messages, shared by every transport
- `internal/session` — byte-offset replay ring, session IDs
- `internal/worker` — PTY ownership, Unix socket, attach/steal, bounded outbound
  queues, resize, signals, kill escalation, `meta.json` lifecycle record
- `internal/terminal` — rendered screen snapshots for clean reattachment (T01)
- `internal/storage` — SQLite session, host, and cached-service store (T03, T14)
- `internal/daemon` — worker discovery, reconciliation, relay, lifecycle,
  signed certificate installation, service-only TLS, and supervised origin/edge
  roles (T04, T12, T13)
- `internal/transport` — WebSocket transport with resume (T05)
- `internal/identity`, `internal/tailnet` — host keys, Tailscale discovery (T06)
- `internal/bootstrap`, `scripts/install` — SSH adoption, release selection,
  identity verification, and idempotent systemd/launchd installers (T08)
- `internal/cli`, `cmd/mesh` — Cobra + Fang product surface, versioned host
  address book, concurrent live/cached host catalogs, remote create, attach,
  logs, kill, signals, the T09 picker boundary, and service preview,
  publication, listing, caching, and removal (T07, T14)
- `internal/tui` — Bubble Tea host/session picker with live/stale state, wake,
  new, resume, attach, and terminal-safe raw-mode handoff (T09)
- `.github`, `.goreleaser.yaml`, `scripts/install` — reproducible release
  archives, checksum-gated installers, systemd/launchd services, Homebrew Cask,
  immutable CI actions, and retained packaging checks (T10)
- `internal/serve` — durable static, files, and loopback-proxy services; hardened
  shared root resolution; bounded public-directory inspection; live service
  controls and restart restoration (T11, T14)
- `internal/dnsname` — Cloudflare-owned private A/TXT reconciliation, bounded
  RFC 8555 DNS-01 issuance, atomic live/staging wildcard state, signed origin
  and private-name distribution, public-edge certificate isolation, and
  supervised Tailscale address rebinding (T12, T13, T14)
- `internal/edge` — signed complete route snapshots, durable ownership and
  liveness, authenticated status pages, bounded proxy/direct-TLS front doors,
  and origin publication with exact acknowledgement (T13)

Verified 2026-08-30: `gofmt`, `go vet ./...`, `go test -race ./...`, and all seventeen
scripts in `integration/` passing.

Private origin names and wildcard certificates are operational, including
staging isolation and hot live rotation. The public edge is operational in both
loopback proxy and direct-TLS modes, including restart/offline ownership and
profile-isolated certificates. Real Cloudflare, Let's Encrypt, domain, tailnet,
and outside-tailnet acceptance remains an operator check because this
development machine has none of those credentials or peers. Step 9's design is
written; T15 implementation has not started.

## Complete tasks

T01 vt snapshot · T02 outbound queue · T03 storage · T04 daemon · T05 websocket
transport · T06 host identity · T07 CLI surface · T08 ssh bootstrap · T09 picker
TUI · T10 packaging · T11 serving core · T12 private names · T13 public edge ·
T14 `mesh serve`.

## Next

```
T15 SSH front door ──┬──→ T16 SFTP and SCP
                     ├──→ T17 sessions over SSH
                     └──→ T18 reverse tunnels
```

T15 is unblocked. T16, T17, and T18 wait on it.

| Task | Owns | Blocked by |
|---|---|---|
| T15 SSH front door | `internal/sshd/` | — |
| T16 SFTP and SCP | `internal/sshfs/` | T11, T15 |
| T17 sessions over SSH | `internal/sshd/session.go` | T09, T15 |
| T18 reverse tunnels | `internal/tunnel/`, claim adapters | T13, T15 |

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
