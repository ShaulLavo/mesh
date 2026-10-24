# T04 — `mesh daemon`

**Status:** complete · **Blocked by:** T03, T06 · **Owns:** `internal/daemon/`

## Goal

A per-host coordinator that discovers and tracks session workers, owns the SQLite
store, and serves clients — local and remote — over the shared protocol.

## The thing that must not break

The daemon must be killable at any moment without touching a single session.
Everything it knows is rediscoverable from the session directories. It holds no
PTY, no process, and no state that is not reconstructible.

Test this explicitly, not by inspection.

## Responsibilities

1. **Discovery.** Scan `<state dir>/s/*`, dial each socket, reconcile each
   `meta.json` into SQLite. A directory whose socket does not answer and whose
   meta says `running`: if the recorded boot ID differs from the current one, it
   is `interrupted`; if it matches, the worker was killed — also `interrupted`.
2. **Session creation.** Spawn detached workers (the logic in
   `internal/cli.Spawn` moves here; the CLI should ask the daemon rather than
   spawn directly, once a daemon exists).
3. **Relay.** Accept a client connection, and for each `session.attach` proxy
   frames between that client and the worker's Unix socket. The framing is
   identical on both sides — this is a copy loop, not a translation layer (D2).
4. **Lifecycle.** Serve `session.list`, `session.kill`, `session.signal`, and
   host info without attaching.
5. **Its own socket** at `<state dir>/daemon.sock`, plus (T05) a WebSocket
   listener bound to the Tailscale address.

## Design notes

- Watch for worker exit by watching `meta.json` (fsnotify or a short poll — poll
  is fine and has fewer failure modes; justify whichever you pick).
- A daemon restart mid-attach drops client connections. That is allowed: clients
  reconnect. What is not allowed is the worker noticing or the session changing
  state.
- Keep `internal/cli`'s direct-to-worker path working. It is the recovery mode for
  when the daemon is broken, and the integration scripts depend on it.

## Acceptance

- `integration/daemon_restart.sh`: start a session through the daemon, `kill -9`
  the daemon, restart it, assert the session is still `running`, still the same
  PID, and still attachable with correct output.
- `integration/reboot_simulation.sh`: fake a boot-ID change, assert the session is
  reported `interrupted` and not resurrected.
- Existing integration scripts still pass.

## Out of scope

WebSocket listener (T05 provides the transport; wire it once both land), host
aliases and multi-host `ls` (T07).
