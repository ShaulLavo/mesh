# Status

Updated 2026-08-29.

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
- `internal/cli`, `cmd/mesh` — `daemon`, `local`, `attach`, `ls`, `logs`, `kill`,
  `sig`, `session-worker`

Verified 2026-08-29: `gofmt`, `go vet ./...`, `go test -race ./...`, and all six
scripts in `integration/` passing.

## Complete tasks

T01 vt snapshot · T02 outbound queue · T03 storage · T04 daemon · T05 websocket
transport · T06 host identity.

## Next

```
T07 CLI surface ──→ T09 picker TUI
             └───→ T10 packaging
T08 ssh bootstrap
T11 serving core ──┬─→ T12 private names ──→ T13 public edge ──→ T14 m serve
                   └─────────────────────────────┘
```

T07 and T08 are unblocked and own disjoint files, so they can run in parallel.
T11 is unblocked by T04 and owns a new package, so it can run alongside both.
T09, T10, T12 and T13 wait on those.

| Task | Owns | Blocked by |
|---|---|---|
| T07 CLI surface | `cmd/mesh/`, `internal/cli/` | — |
| T08 ssh bootstrap | `internal/bootstrap/`, `scripts/install/` | — |
| T11 serving core | `internal/serve/` | — |
| T09 picker TUI | `internal/tui/` | T07 |
| T10 packaging | `.github/`, `.goreleaser.yaml` | T07 |
| T12 private names | `internal/dnsname/` | T11 |
| T13 public edge | `internal/edge/` | T11, T12 |
| T14 `m serve` | `internal/cli/` | T11, T12, T13, T07 |

T07 and T14 both own `internal/cli/`. Land T07 first.

## Rules for anyone picking up a task

- Read `CLAUDE.md` and `docs/plan/01-decisions.md` first.
- All integration scripts must still pass. Add one if you add behaviour worth
  protecting.
- Do not change `internal/protocol` without saying so loudly: every component
  shares it. Additive fields are fine, renames are not.
- Update this file and the task brief when you land.
