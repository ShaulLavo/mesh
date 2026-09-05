# Status

Updated 2026-09-05.

## Done

The session and transport core, live session inspector, terminal-window entry, product CLI,
Tailscale-provisioning SSH bootstrap, packaging, origin serving core and service
catalog, private DNS/TLS path, authenticated public edge, and the locked SSH
front door and sessions over SSH are complete. Target-authorized wake, automatic
LAN sender selection, Linux/macOS sleep inhibition, and workspace crash recovery
are also implemented. Physical power-loss acceptance remains an operator check.

- `internal/protocol` — framing, control messages, bounded structured preview
  styles, and exact host/session containment paths shared by every transport
- `internal/session` — byte-offset replay ring, session IDs
- `internal/worker` — PTY ownership, Unix socket, attach/steal, bounded outbound
  queues, resize, signals, kill escalation, live process inspection, validated
  attachment-containment state, and `meta.json` lifecycle records
- `internal/terminal` — rendered screen snapshots for clean reattachment and
  bounded previews with structured terminal styles and OSC title/directory
  metadata (T01, T22)
- `internal/storage` — SQLite session, host, and cached-service store (T03, T14)
- `internal/daemon` — worker discovery, reconciliation, relay, lifecycle,
  signed certificate installation, service-only TLS, a bounded public request
  read deadline, and supervised origin/edge roles (T04, T12, T13)
- `internal/transport` — WebSocket transport with resume (T05)
- `internal/identity`, `internal/tailnet` — host keys, Tailscale discovery, and
  persistent raw TCP/443 forwarding verification (T06, T12)
- `internal/bootstrap`, `scripts/install` — SSH adoption, release selection,
  Tailscale provisioning, separate stdin-only auth-key and sudo-password paths,
  bounded remote output, identity verification, development-build version
  safety, and idempotent systemd/launchd installers (T08, T20)
- `internal/cli`, `cmd/mesh` — Cobra + Fang product surface, versioned host
  address book, concurrent live/cached host catalogs, remote create, attach,
  logs, kill, signals, live session inspection, launch-directory listing, the
  picker boundary, exact nested-containment discovery and pre-picker capture,
  and service preview, publication, listing, caching, and removal (T07, T09,
  T14, T22)
- `internal/tui` — Bubble Tea host/session picker with live/stale state, wake,
  new, resume, attach, selected-session details, current-screen preview, and
  frozen pre-picker previews for every known containing session, plus
  terminal-safe raw-mode handoff (T09, T22)
- Terminal entry — silent `mesh --window`, detached-only `--take`, compact
  resume prompt, local-first asynchronous picker, live nested detach ownership,
  interrupted-session relaunch and durable forgetting, and terminal setup
  instructions (T23)
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
  shutdown, with local sessions, the shared picker, and scriptable `ls` (T15, T17)
- `internal/recovery` — worker-owned rendered checkpoints, shell directory and
  history hooks, durable restart reservations, retained attempts, exact remote
  continuation, explicit command recipes, and previous-output access (T24)
- `internal/wake`, `internal/wakeclient`, `internal/inhibit` — signed target
  permission, automatic LAN sender selection, conservative reconnect recovery,
  public-service wake, and child-process sleep inhibition (T19)

Verified 2026-08-30: clean formatting and module/generation diffs, the unlimited
seven-linter policy, `go vet ./...`, `go test -race ./...`, all three retained
parser fuzz targets, all eighteen scripts in `integration/`, and CGO-disabled
Linux amd64/arm64 and Darwin arm64 builds passing.

Verified 2026-09-01 after T20: clean module and formatting diffs, the full
seven-linter policy, `go vet ./...`, `go test -race ./...`, and all twenty
scripts in `integration/`. `scripts/check-t20.sh` also verifies the focused
provisioning contract and the installer harness.

Verified 2026-09-05 after T22: clean module and formatting diffs, the focused
T22 contract and Linux/Darwin cross-builds, change-scoped seven-linter checks,
`go vet ./...`, `go test -race ./...`, and all twenty-one scripts in
`integration/`. The retained session-inspection regression crosses a real
WebSocket, daemon lifecycle, Unix worker, process observer, and terminal
emulator without attaching or resizing the session. The integration runner
also isolates host configuration and Mesh nesting identity for each script, so
running it from inside a session cannot change detach keys or catalog output.

T22's current contract also carries the immediate-to-outer attachment path
through every attach, obtains that path from the containing worker, captures
each known containing screen before the picker renders, and preserves terminal
presentation as validated structured runs rather than raw ANSI. Focused tests
cover path bounds and cycles, rejection before attachment takeover, legacy
single-session fallback, frozen containing-session previews, plain-preview
fallback, and style isolation at the panel boundary.

Verified 2026-09-05 after T23: clean module and formatting diffs,
`go vet ./...`, `go test -race ./...`, all twenty-six scripts in
`integration/`, and CGO-disabled Linux amd64/arm64 and Darwin arm64 builds.
`scripts/check-t23.sh` retains the focused checks and five real-PTY scenarios,
including loopback WebSocket nesting, concurrent window claims, terminal crash
recovery, and interrupted-session relaunch with online and offline retirement.
Ghostty 1.3.1 validated the documented terminal command. Change-scoped checks
pass all seven linters; the full lint run reports eleven existing findings in
unchanged bootstrap, installer, and tag files.

T23 narrows pre-picker containment capture to local screens so local rendering
never waits for a remote host. Remote containing identities retain unavailable
frozen previews for that picker instance; ordinary remote inspections still
load on selection. Legacy detach hints go to stderr and leave terminal output
byte-for-byte intact.

Runtime feature availability normally follows the worker process version, not
only the installed binary version. The Linux and Darwin service definitions
preserve session workers when an installer restarts the daemon. For an older
worker that returns a plain inspection, the new daemon replays only that
worker's bounded raw ANSI stream at its observed PTY size and uses recovered
styles only when every row matches the authoritative inspection. No guessed
highlighting is applied, and the client never receives raw controls. A failed
match falls back to escaped plain text. A worker or daemon that predates
inspection yields an unavailable live view. A mixed-version containment path
supplies only the exact prefix available from chain-aware workers and the proven
immediate-session fallback; none of these cases attaches to or mutates the
inspected session.

Private origin names and wildcard certificates are operational, including
staging isolation and hot live rotation. The public edge is operational in both
loopback proxy and direct-TLS modes, including restart/offline ownership and
profile-isolated certificates. Real Cloudflare, Let's Encrypt, domain, tailnet,
and outside-tailnet acceptance remains an operator check because this
development machine has none of those credentials or peers. Step 9's shared
front door and session handler are complete. Its file and tunnel handlers remain.

Verified 2026-09-05 after T17: `go mod tidy -diff`, formatting checks,
`go vet ./...`, `go test -race ./...`, all 27 integration scripts, and
CGO-disabled Linux amd64/arm64 and Darwin arm64 builds. The stock OpenSSH
scenario verifies picker/attach handoff, resize, takeover across local and SSH
clients, shell survival after killing SSH, new sessions, and exit statuses.
SSH tests retain a 100-cycle goroutine check, independent channel cancellation,
and input EOF while output is blocked by an unread SSH channel.
All seven linters pass on changed lines. Full lint still reports eleven existing
findings in unchanged bootstrap, installer, and tag files.

The final review also preserves `--isolate` in cached service catalogs through
database migration 6 and a close/reopen regression. Client-side containment
checks protect preserved legacy workers from self-attachment. Nesting capability
propagates through the full attachment chain, and workers reject takeovers that
would break existing nested detach keys.

The integration run also exposed an exit race in one-shot kill requests. Workers
now wait for admitted kill responders to send their bounded acknowledgements
before returning, so process shutdown cannot discard a successful kill response.

## Complete tasks

Verified 2026-09-05 after T19: `go test -race ./...`, `go vet ./...`, clean module
and formatting checks, all 28 integration scripts, and CGO-disabled Linux
amd64/arm64 plus Darwin arm64 builds. `scripts/check-t19.sh` retains the focused
contract. Full lint reports only the same eleven findings in unchanged
bootstrap, installer, and tag files; changed and new code introduces none.
Real WebSocket tests cover permission and revocation; real process tests prove
inhibitor release after daemon SIGKILL. Physical wake/suspend and macOS power
assertions remain checks on the actual machines described in `docs/power.md`.

T01 vt snapshot · T02 outbound queue · T03 storage · T04 daemon · T05 websocket
transport · T06 host identity · T07 CLI surface · T08 ssh bootstrap · T09 picker
TUI · T10 packaging · T11 serving core · T12 private names · T13 public edge ·
T14 `mesh serve` · T15 SSH front door · T17 sessions over SSH · T19 wake and sleep inhibition · T20 Tailscale provisioning · T22 live
session inspector · T23 Mesh as your terminal · T24 workspace crash recovery.

Verified 2026-09-05 after T24: `go test -race ./...`, `go vet ./...`, clean module
and formatting checks, all 34 integration scripts, and CGO-disabled Linux
amd64/arm64 plus Darwin arm64 builds. `scripts/check-t24.sh` retains the focused
contract and real Bash/Zsh, WebSocket, and OpenSSH recovery scenarios. A daemon
response-ordering regression also covers attachment rejection followed by resize.
Full unlimited lint reports only the same eleven pre-existing findings.
Physical power-loss acceptance and a wider real-process crash-boundary matrix
remain [documented validation limits](../tasks/T24-crash-recovery.md#delivery-notes).

## Build order coverage

Tasks are the unit of work; the ten build-order steps in `00-overview.md` are
the unit of completeness. Track both. A step with no task is invisible to a
task list, which is how step 6 stayed unbuilt while every task was green.

| Step | Tasks | State |
|---|---|---|
| 1 Local persistent session | T01, T02, T03, T04 | complete |
| 2 Remote session over Tailscale | T05, T06 | complete |
| 3 Product CLI | T07, T09, T22 | complete |
| 4 SSH bootstrap | T08, T20, T21 | complete |
| 5 Crash and restart recovery | T04, T24, T25 | daemon and workspace recovery implemented; agent recovery planned |
| 6 Wake and sleep inhibition | T19 | complete |
| 7 Packaging | T10 | complete |
| 8 Serving | T11, T12, T13, T14 | complete |
| 9 SSH front door | T15, T16, T17, T18 | T15, T17 complete |
| 10 Mesh as your terminal | T23 | complete |

## Next

T16 and T18 remain independent and unblocked. T24 provides the checkpoint and
restart operation needed by T25. The offline provider probe is implemented; live
invocation association and exact native resume still need verification.

| Task | Owns | Blocked by |
|---|---|---|
| T16 SFTP and SCP | `internal/sshfs/` | T11, T15 |
| T18 reverse tunnels | `internal/tunnel/`, claim adapters | T13, T15 |
| [T25 Codex and Claude recovery](../tasks/T25-agent-recovery.md) | `internal/agentresume/`, exact conversation registration, native resume | live provider association and exact-resume verification |

T24 recovers a shell in its last saved directory, retains previous output and
attempts, and resolves exact remote targets from the Mesh CLI. SSH recovery stays
on its connected host. `mesh logs SESSION --previous` reads retained output.
T25 adds no runtime behavior yet; its offline probe does not establish automatic
conversation association. See [recovery instructions](../recovery.md).

T19 closes the skipped power-control step. D25 and D26 record child inhibitors,
target-owned permission, automatic LAN sender selection, and the additive wake
protocol. The CLI, picker, and public edge now use the wake implementation.

Next for agent recovery, prove invocation association and exact native resume
with authenticated disposable Codex and Claude conversations. Then add T25
registration and provider launch on T24's shared restart operation. T16 and T18
can proceed independently.

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
