# Mesh — working agreement

Mesh gives you direct, resumable terminal sessions across your Tailscale-connected
machines. Read `docs/plan/00-overview.md` before changing anything structural, and
`docs/plan/01-decisions.md` before re-litigating a design choice.

## Invariants (do not break these)

1. Connections are disposable. Sessions belong to the host, not the client.
2. Losing wifi, closing the laptop, or killing `mesh` must not kill the command.
3. A daemon crash or upgrade must not kill any session. Workers own PTYs; the
   daemon coordinates.
4. Terminal traffic goes host-to-host. The Pi wakes and discovers; it never proxies.
5. A rebooted host reports its sessions as `interrupted`. Mesh does not pretend to
   resurrect RAM.
6. Each host is authoritative for its own sessions. There is no central database.

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
./scripts/verify.sh
```

`scripts/verify.sh` builds one temporary binary and runs all integration scripts
concurrently. Each script uses a separate state directory. Any change to session
lifecycle, attach, or kill must keep all scripts passing. Add a script for new
behavior that needs protection.

## Layout

- `internal/protocol` — the wire format. Transport agnostic; shared by the Unix
  socket and (later) the WebSocket. Changing it affects every component.
- `internal/session` — session state primitives (output ring, IDs).
- `internal/worker` — `mesh session-worker`: one PTY, one process, one socket.
- `internal/cli` — the client half: attach, discovery, spawn, control.
- `internal/paths` — where state lives on disk.
- `cmd/mesh` — mode dispatch.
