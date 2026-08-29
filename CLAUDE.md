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
go test -race ./...
go build -o mesh ./cmd/mesh
./integration/survives_client_death.sh    # the contract, end to end
./integration/detach_and_steal.sh
```

Integration scripts set `MESH_STATE_DIR` to a temp dir. Any change to session
lifecycle, attach, or kill must keep both scripts passing, and behaviour worth
protecting gets a new script rather than a comment saying it works.

## Layout

- `internal/protocol` — the wire format. Transport agnostic; shared by the Unix
  socket and (later) the WebSocket. Changing it affects every component.
- `internal/session` — session state primitives (output ring, IDs).
- `internal/worker` — `mesh session-worker`: one PTY, one process, one socket.
- `internal/cli` — the client half: attach, discovery, spawn, control.
- `internal/paths` — where state lives on disk.
- `cmd/mesh` — mode dispatch.
