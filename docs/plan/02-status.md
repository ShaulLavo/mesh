# Status

Updated 2026-08-29.

## Done — step 1, the heart

Commit `774a641` established the local session core. T02 now hardens its output
path. A detached worker owns one PTY and outlives every client.

- `internal/protocol` — framing + control messages, shared by all transports
- `internal/session` — byte-offset replay ring (randomized tests), session IDs
- `internal/worker` — PTY ownership, Unix socket, attach/steal, bounded outbound
  queues, resize, signals, kill escalation, `meta.json` lifecycle record
- `internal/cli` — attach client, raw mode, SIGWINCH, detach key, spawn, discovery
- `cmd/mesh` — `local`, `attach`, `ls`, `logs`, `kill`, `sig`, `session-worker`

Verified: `go test -race ./...`, plus the three scripts in `integration/`, each
run repeatedly.

## Known gaps in what exists

The remaining gap is written up as a task.

- Reattach replays raw bytes, not a rendered screen. Inside a full-screen program
  you get garbage. → `docs/tasks/T01-vt-snapshot.md`

## Delegation map

```
T01 vt snapshot ─────→ finish step 1
T02 outbound queue ✓
T03 storage ─────────→ T04 daemon ──→ T07 CLI surface ──→ T09 picker
T05 websocket ─────────→ T04           T08 ssh bootstrap
T06 host identity ─────→ T04
```

Safe to run in parallel right now: **T01, T03, T05, T06.** They own disjoint
files. T04 needs T03 and T06 landed; T07 needs T04. T02 is complete.

| Task | Owns | Blocked by |
|---|---|---|
| T01 vt snapshot | `internal/terminal/`, `worker/serve.go` | — |
| T02 outbound queue (complete) | `worker/worker.go`, `worker/serve.go`, `cmd/mesh/main.go` | — |
| T03 storage | `db/`, `internal/storage/` | — |
| T05 websocket transport | `internal/transport/` | — |
| T06 host identity | `internal/identity/`, `internal/tailnet/` | — |
| T04 daemon | `internal/daemon/` | T03, T06 |
| T07 CLI surface | `cmd/mesh/`, `internal/cli/cmd*` | T04 |
| T08 ssh bootstrap | `internal/bootstrap/`, `scripts/install/` | T04, T06 |
| T09 picker TUI | `internal/tui/` | T07 |
| T10 packaging | `.github/`, `.goreleaser.yaml` | T07 |

T01 can now build on T02's ordered attachment queue.

## Rules for anyone picking up a task

- Read `CLAUDE.md` and `docs/plan/01-decisions.md` first.
- All integration scripts must still pass. Add one if you add behaviour worth
  protecting.
- Do not change `internal/protocol` without saying so loudly: every component
  shares it. Additive fields are fine, renames are not.
- Update this file and the task brief when you land.
