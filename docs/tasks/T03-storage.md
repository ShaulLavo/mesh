# T03 — SQLite session store

**Status:** not started · **Blocked by:** nothing · **Owns:** `db/`, `internal/storage/`

## Goal

Per-host durable metadata, so `mesh ls` is fast, offline hosts can be cached, and
session history survives daemon restarts. Metadata only — never the live process.

## Design

- `modernc.org/sqlite` (cgo-free, so cross-compiling for the Pi stays trivial).
- Goose migrations in `db/migrations/`, sqlc queries in `db/queries/`, generated
  code into `internal/storage/`.
- Database at `<state dir>/mesh.db` (see `internal/paths`). WAL mode.
- **The daemon is the only writer.** Workers record their own fate in
  `meta.json`; the daemon reconciles those files into SQLite on startup and on
  change. See decision D7.

Schema, per the plan:

```
sessions: id, host_id, command, cwd, state, created_at, last_attached_at,
          exit_code, last_output_sequence
hosts:    id, alias, mesh_identity, tailscale_name, last_seen_at
```

`state` is one of `running`, `detached`, `exited`, `interrupted`. Store what was
last *observed*; liveness is derived at read time by the daemon, never trusted
from the row alone.

## Acceptance

- `go generate ./...` (or a documented make target) regenerates sqlc output.
- Migrations apply from empty and are idempotent on re-run.
- Go tests against a temp DB covering: insert, state transition, list-by-state,
  and reconciling a `running` row whose worker is gone into `interrupted`.
- No `database/sql` handles leak; `go test -race` clean.

## Out of scope

Cross-host merge logic for `m ls` (that is T07), and any daemon wiring (T04).
Define the store and its queries; leave the caller to T04.

## Notes

Add the tool versions (goose, sqlc) to a `tools.go` or the README so the next
person regenerates with the same versions.
