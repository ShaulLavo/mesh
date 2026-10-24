# Mesh working agreement

Mesh gives you direct, resumable terminal sessions across your Tailscale-connected
machines. Read `docs/concepts/architecture.md` before changing anything structural,
and the matching file in `docs/adr/` before re-litigating a design choice.

## Invariants (do not break these)

1. Connections are disposable. Sessions belong to the host, not the client.
2. Losing wifi, closing the laptop, or killing `mesh` must not kill the command.
3. A daemon crash or upgrade must not kill any session. Workers own PTYs; the
   daemon coordinates.
4. Terminal traffic goes host-to-host. The Pi wakes and discovers; it never proxies.
5. A rebooted host reports its sessions as `interrupted`. Mesh does not pretend to
   resurrect RAM.
6. Each host is authoritative for its own sessions. There is no central database.

The explicit `mesh ssh HOST [-- command...]` mode owns an ordinary process for
one SSH connection. It ends on disconnect and creates no persistent session,
worker, replay, or recovery state. The session invariants above apply to the
default persistent mode.

## Conventions

- Go 1.27. `gofmt` clean, `go vet ./...` clean, `go test -race ./...` green.
- Comments explain *why*, not what. Do not narrate the code.
- Errors wrap with `%w` and name the thing that failed (`session 7K3D: ...`).
- Charm's `x/*` packages are pinned and used behind our own types, so their API
  churn stays contained.
- No new dependency without a line in the task brief justifying it.

## Testing

```bash
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/gates.sh
./scripts/verify.sh
```

`scripts/verify.sh` builds one temporary binary and runs all integration scripts
concurrently. Each script uses a separate state directory. Any change to session
lifecycle, attach, or kill must keep all scripts passing. Add a script for new
behavior that needs protection. `scripts/gates.sh` runs the lint baseline; see
`docs/development.md` before editing `.gates/baseline.json`.

## Layout

`docs/concepts/architecture.md` maps the main packages. The ones most changes touch:

- `internal/protocol`: the wire format, shared by the Unix socket, the WebSocket
  and SSH. Changing it affects every component.
- `internal/session`: session state primitives (output ring, IDs).
- `internal/worker`: `mesh session-worker`: one PTY, one process, one socket.
- `internal/cli`: the client half: attach, discovery, spawn, control.
- `internal/paths`: where state lives on disk.
- `cmd/mesh`: mode dispatch.

## Docs

- `docs/reference/cli.md` is generated. After changing a command or its help text, run
  `python3 scripts/gen-cli-reference.py > docs/reference/cli.md`.
- `docs/` holds user and contributor docs only. Put plans, task notes, audits and
  measurements in `.internal/` and never link to `.internal/` from `docs/` or the README.
- Record a design choice with a non-obvious reason as `docs/adr/NNN-title.md`.
