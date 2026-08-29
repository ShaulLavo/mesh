# T03 — SQLite session store

**Status:** complete · **Blocked by:** nothing · **Owns:** `db/`, `internal/storage/`

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

## Implementation

`storage.Open(ctx, databasePath)` accepts an explicit database file, enables WAL
and foreign keys on every connection, and applies embedded Goose migrations.
The schema uses `(host_id, id)` as the session key because session IDs are unique
only within one host. Commands are JSON arrays, and timestamps are Unix
milliseconds.

The public package exposes Mesh types rather than generated SQL types. Host IDs
and session IDs have distinct Go types. Session states are constants, and the
store rejects invalid state and exit-code combinations before a query runs.

`Store.ReconcileHost` is the startup boundary for T04. The caller must provide a
complete, authoritative scan for one host. In one transaction, the method marks
stored `running` and `detached` rows that are missing from the scan as
`interrupted`, then upserts the observed rows. It preserves exited history and
never decreases `last_output_sequence`. The host observation and its session
view commit in the same transaction, so a failed reconciliation changes neither.

Goose is pinned at v3.27.3 in `go.mod`. `db/generate.go` pins sqlc v1.31.1 and
regenerates `internal/storage/sqlc`:

```bash
go generate ./...
```

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

The sqlc output is committed. Do not edit `internal/storage/sqlc` by hand.
