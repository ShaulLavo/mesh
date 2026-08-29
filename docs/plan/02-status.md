# Status

Updated 2026-08-29.

## Done — step 1, the heart

Commit `774a641`. A detached worker owns one PTY and outlives every client.

- `internal/protocol` — framing + control messages, shared by all transports
- `internal/session` — byte-offset replay ring (randomized tests), session IDs
- `internal/worker` — PTY ownership, Unix socket, attach/steal, resize, signals,
  kill escalation, `meta.json` lifecycle record
- `internal/cli` — attach client, raw mode, SIGWINCH, detach key, spawn, discovery
- `cmd/mesh` — `local`, `attach`, `ls`, `logs`, `kill`, `sig`, `session-worker`

Verified: `go test -race ./...`, plus `integration/survives_client_death.sh` and
`integration/detach_and_steal.sh`, each run repeatedly.

## Known gaps in what exists

Both are written up as tasks; neither is a surprise.

- Reattach replays raw bytes, not a rendered screen. Inside a full-screen program
  you get garbage. → `docs/tasks/T01-vt-snapshot.md`
- The PTY pump can block up to 5s on a wedged client before disowning it. Wrong
  over a flaky link. → `docs/tasks/T02-outbound-queue.md`

## Delegation map

```
T01 vt snapshot ──┐
T02 outbound queue ┼─→ finish step 1
T03 storage ───────┴─→ T04 daemon ──→ T07 CLI surface ──→ T09 picker
T05 websocket ─────────→ T04           T08 ssh bootstrap
T06 host identity ─────→ T04
```

Safe to run in parallel right now: **T01, T02, T03, T05, T06.** They own disjoint
files. T04 needs T03 and T06 landed; T07 needs T04.

| Task | Owns | Blocked by |
|---|---|---|
| T01 vt snapshot | `internal/terminal/`, `worker/serve.go` | — |
| T02 outbound queue | `worker/worker.go` | — |
| T03 storage | `db/`, `internal/storage/` | — |
| T05 websocket transport | `internal/transport/` | — |
| T06 host identity | `internal/identity/`, `internal/tailnet/` | — |
| T04 daemon | `internal/daemon/` | T03, T06 |
| T07 CLI surface | `cmd/mesh/`, `internal/cli/cmd*` | T04 |
| T08 ssh bootstrap | `internal/bootstrap/`, `scripts/install/` | T04, T06 |
| T09 picker TUI | `internal/tui/` | T07 |
| T10 packaging | `.github/`, `.goreleaser.yaml` | T07 |

T01 and T02 both live in `internal/worker` but in different files; land T02 first
if one agent is doing both, since it changes how output reaches a client.

## Rules for anyone picking up a task

- Read `CLAUDE.md` and `docs/plan/01-decisions.md` first.
- The two integration scripts must still pass. Add one if you add behaviour worth
  protecting.
- Do not change `internal/protocol` without saying so loudly: every component
  shares it. Additive fields are fine, renames are not.
- Update this file and the task brief when you land.
