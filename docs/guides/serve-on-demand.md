# Serve a dev server on demand

The examples use `new.example` from the configured
[deployment domains](../reference/deployment-domains.md). Replace it with your domain.

An on-demand route names the command behind a port. Mesh starts that command as
a session when the first connection through Mesh arrives, and stops it once no
such connection has been open for the idle window. Agents, browsers and scripts on
the host all share the one process, and nobody starts or stops it by hand.

A route with `--keep-running` keeps its command running instead: it starts at
once, never stops for idleness, and starts again after the command exits. Use it
for a service that clients reach on its upstream port directly. See
[Kept routes](#kept-routes).

```bash
mesh serve pc --run 'bun run dev:web' --cwd ~/projects/app \
  --listen 5173=15173 --listen 3001=13001 --idle 15m
```

Point the dev server at the upstream ports (15173 and 13001 here), bound to
`127.0.0.1`. Clients use the listener ports (5173 and 3001). Mesh binds those on
`127.0.0.1` and proxies HTTP and WebSocket traffic from each one to its upstream.

## The route

| Flag | Meaning |
|---|---|
| `--run CMD` | The command. It runs through your login shell, so it finds the same tools a terminal would. |
| `--cwd DIR` | Where it starts. On this machine it defaults to the directory you ran `mesh serve` from. Another machine needs it named; a relative path there is under that user's home. |
| `--env KEY=VALUE` | Added to the daemon's environment. Repeatable. |
| `--listen PUBLIC=UPSTREAM` | A loopback listener on the host. Repeatable. |
| `--at /path` | Also serve the route on the tailnet at `/path`. Optional when `--listen` is given; without it `mesh serve ls` shows SCOPE `local`. |
| `--idle 15m` | How long the route runs with no open connection through Mesh. The default for a new route is 15 minutes. `--idle 0` is refused; use `--keep-running`. |
| `--keep-running` | Start now, never stop for idleness, restart after an exit. It cannot be combined with `--idle`, and `--keep-running=false` is refused: choose `--idle` instead. |
| `--ready-timeout 60s` | How long a starting route holds connections before failing them. |

Running `mesh serve` again for an existing route keeps what you leave out: its
lifetime unless `--idle` or `--keep-running` is given, and its ready timeout
unless `--ready-timeout` is given. Mesh reads both from the host itself, so the
host must be reachable.

A route with no `--at`, or with `--at :5173`, is reached only through its
listeners. It is named by the listener port it is reached on, `:5173`: TARGET,
or the first `--listen` port when TARGET is left out. With `--at /dev`, the
route is `/dev`, and a request to `https://pc.mesh.new.example/dev/` starts it
too. When TARGET is one of the listener ports, the tailnet path forwards
straight to that listener's upstream.

On-demand routes are private, like every Mesh service.

## What happens

- **First connection.** The daemon starts the command as a session labelled
  `serve /dev` (or `serve :5173`). `mesh ls` shows the label in the TITLE column,
  and `mesh ID` attaches to the session's output. Connections wait until every
  upstream port accepts.
- **Idle.** Every open connection through Mesh counts: keep-alives, WebSockets,
  and requests on the tailnet path. A client that connects to the upstream port
  directly does not count, and cannot start a stopped route. When the count
  reaches zero the idle clock starts, and any new connection cancels it. When the
  clock runs out, the session is stopped the way `mesh kill` stops one. The
  listeners stay bound. The daemon log names the stopped session and the idle
  window.
- **Failure.** If the command exits before its ports accept, or the ready timeout
  passes, the waiting connections get a 502 with a reference. The daemon log has
  the route, the exit status, and the last lines of output under that reference.
  The route shows `failed` until the next connection tries again. A route with an
  idle window is not restarted until then.
- **Daemon restart.** The session keeps running. The new daemon binds the
  listeners again, adopts the session by its label, and starts a full idle
  window. After a reboot the next connection starts a fresh session.
- **History.** Each pruning pass keeps up to eleven eligible historical exits:
  the ten newest attempts and the newest one that ran at least a minute. The
  current attempt is additional. Interrupted, recovery-protected and app-pinned
  records are additional and are never removed this way. Eligible exits also
  have a seven-day age limit from exit and a host-wide 500-record cap. Pressure
  or churn can remove history earlier; protected records can outlive both limits.
  Use `mesh ls --all` and `mesh logs` to inspect retained attempts.

A `--listen` port that something else already holds is refused, and the error
names the process holding it. So is one another route already listens on. Mesh
never picks another port.

Running `mesh serve` again with a new command, directory or environment stops
the running session; the next connection starts the new recipe. A new lifetime
or ready timeout alone leaves it running with the same session.

## Kept routes

```bash
mesh serve pc 9000 --at /models --run ./run --cwd /srv/models \
  --listen 9000=19000 --keep-running
```

- The command starts as soon as the route is saved, with no traffic.
- It never stops for idleness. Connections are still counted for `mesh serve ls`.
- When it exits, for any reason and with any status, Mesh starts it again after
  1 second, then 2, 4 and so on, up to 5 minutes between tries. A run of at least
  a minute starts the delay over. Requests during the delay get a 502 and do not
  start it sooner.
- Clients may use the upstream port directly. Mesh does not watch that traffic,
  and it cannot keep the port answering while the command starts or restarts:
  those clients see refused connections then, and should retry.
- `mesh serve stop` stops it and keeps it stopped: `mesh serve ls` shows
  `stopped (held)`. It stays stopped until `mesh serve start`, a change of the
  route's definition, or a daemon restart. A new display name alone keeps it
  stopped.
- `mesh serve start` starts it now, without waiting for the restart delay.

## Commands

```bash
mesh serve ls                # STATE: stopped · stopped (held) · starting · running (3 conns) · failed
mesh serve start :5173       # start now and wait until it is ready
mesh serve stop :5173        # stop now; the next connection starts it again, or a kept route stays stopped
mesh unserve :5173           # stop it, release the ports, forget the route
```

A browser tab with a live HMR socket keeps the route running. The daemon closes
keep-alive connections that have been idle for two minutes, so a tab without a
live socket lets the route stop.
