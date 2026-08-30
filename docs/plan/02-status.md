# Status

Updated 2026-08-30.

## Done

The session and transport core, product CLI, SSH bootstrap, packaging, origin
serving core and service catalog, private DNS/TLS path, authenticated public
edge, and the locked SSH front door are complete.

- `internal/protocol` — framing + control messages, shared by every transport
- `internal/session` — byte-offset replay ring, session IDs
- `internal/worker` — PTY ownership, Unix socket, attach/steal, bounded outbound
  queues, resize, signals, kill escalation, `meta.json` lifecycle record
- `internal/terminal` — rendered screen snapshots for clean reattachment (T01)
- `internal/storage` — SQLite session, host, and cached-service store (T03, T14)
- `internal/daemon` — worker discovery, reconciliation, relay, lifecycle,
  signed certificate installation, service-only TLS, a bounded public request
  read deadline, and supervised origin/edge roles (T04, T12, T13)
- `internal/transport` — WebSocket transport with resume (T05)
- `internal/identity`, `internal/tailnet` — host keys, Tailscale discovery, and
  persistent raw TCP/443 forwarding verification (T06, T12)
- `internal/bootstrap`, `scripts/install` — SSH adoption, release selection,
  development-build version safety, identity verification, and idempotent
  systemd/launchd installers (T08)
- `internal/cli`, `cmd/mesh` — Cobra + Fang product surface, versioned host
  address book, concurrent live/cached host catalogs, remote create, attach,
  logs, kill, signals, the T09 picker boundary, and service preview,
  publication, listing, caching, and removal (T07, T14)
- `internal/tui` — Bubble Tea host/session picker with live/stale state, wake,
  new, resume, attach, and terminal-safe raw-mode handoff (T09)
- `.github`, `.goreleaser.yaml`, `scripts/install` — reproducible release
  archives, checksum-gated installers, systemd/launchd services, Homebrew Cask,
  immutable CI actions, seven-linter boundary checks, and retained packaging
  checks (T10)
- `internal/serve` — durable static, files, and loopback-proxy services; fuzzed
  shared root resolution; `os.Root`-backed file access; bounded public-directory
  inspection; live service controls and restart restoration (T11, T14)
- `internal/dnsname` — Cloudflare-owned private A/TXT reconciliation, bounded
  RFC 8555 DNS-01 issuance, atomic live/staging wildcard state, signed origin
  and private-name distribution, public-edge certificate isolation, and
  supervised Tailscale address rebinding (T12, T13, T14)
- `internal/edge` — signed complete route snapshots, durable ownership and
  liveness, authenticated status pages, fuzzed Host parsing, bounded
  proxy/direct-TLS front doors, and origin publication with exact
  acknowledgement (T13)
- `internal/sshd` — Wish-based key-only SSH on discovered Tailnet addresses,
  the shared Mesh host key, live `authorized_keys` revocation, and daemon-owned
  shutdown (T15)

Verified 2026-08-30: clean formatting and module/generation diffs, the unlimited
seven-linter policy, `go vet ./...`, `go test -race ./...`, all three retained
parser fuzz targets, all eighteen scripts in `integration/`, and CGO-disabled
Linux amd64/arm64 and Darwin arm64 builds passing.

Private origin names and wildcard certificates are operational, including
staging isolation and hot live rotation. The public edge is operational in both
loopback proxy and direct-TLS modes, including restart/offline ownership and
profile-isolated certificates. Real Cloudflare, Let's Encrypt, domain, tailnet,
and outside-tailnet acceptance remains an operator check because this
development machine has none of those credentials or peers. Step 9's shared
front door is complete. Its session, file, and tunnel handlers remain.

## Complete tasks

T01 vt snapshot · T02 outbound queue · T03 storage · T04 daemon · T05 websocket
transport · T06 host identity · T07 CLI surface · T08 ssh bootstrap · T09 picker
TUI · T10 packaging · T11 serving core · T12 private names · T13 public edge ·
T14 `mesh serve` · T15 SSH front door.

## Next

T16, T17, and T18 are independent and unblocked.

| Task | Owns | Blocked by |
|---|---|---|
| T16 SFTP and SCP | `internal/sshfs/` | T11, T15 |
| T17 sessions over SSH | `internal/sshd/session.go` | T09, T15 |
| T18 reverse tunnels | `internal/tunnel/`, claim adapters | T13, T15 |

Pick one of T16, T17, or T18 next.

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

A later public-boundary review closed four more findings:

7. The stable-address limiter was described too strongly and its FIFO let new
   addresses evict live entries. IPv6 clients now share `/64` quotas, saturation
   preserves live entries, and the docs identify global and per-origin
   concurrency as the real protection against address rotation.
8. The public server bounded header time but not body time. It now has a fixed,
   generous 10-minute request-read deadline, returns 408 for an inbound body
   timeout, and deliberately leaves writes unbounded for streaming and
   WebSockets.
9. Static serving and directory scans resolved paths, called `Stat`, and then
   reopened them by name. Actual access now goes through an anchored `os.Root`
   and the same opened descriptor, with a regression that retargets a directory
   outside the root between check and open.
10. `ResolveRoot` and `canonicalPublicHost` now have retained fuzz targets. The
    accompanying parser boundary tests found an encoding-depth off-by-one, and
    the Host fuzz seeds found an accepted empty Host and a rewritten bracketed
    DNS Host; all three are closed.

A final operational and lint review closed six more findings:

11. The explicit lint allowlist omitted `errcheck`, `gosec`, and `bodyclose`,
    while global Staticcheck exclusions hid deprecation and boundary mistakes.
    All three linters and SA1012, SA1019, and SA2001 are active. The packaging
    contract locks that policy, and intentional exceptions are line-scoped.
12. Handler construction contained a panic-only default for an unknown service
    kind. It now returns an error. The same boundary sweep found and closed a
    scheme-relative directory redirect.
13. An unversioned development build could silently download `latest` during a
    cross-platform `mesh add`. It now reuses an exact local executable or sibling
    artifact and otherwise refuses to guess a release version.
14. Direct terminal WebSockets and Tailnet-only HTTP services intentionally rely
    on Tailscale WireGuard and ACLs instead of a second TLS layer. D23 records
    that security boundary and its exposure cost.
15. The Tailscale Serve setup command already ran when requested, but startup did
    not verify operator-managed state. Every private-HTTPS start now requires an
    exact persistent raw TCP/443 forward and rejects TLS/HTTP handling, PROXY
    protocol, Funnel exposure, and foreground shadows before publishing the
    private name.
16. SQLite stores signed 64-bit integers, but edge and session code crossed into
    unsigned sequence types without checking corrupt negative rows. Storage now
    validates every signed/unsigned sequence boundary and rejects values that
    cannot round-trip.

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
