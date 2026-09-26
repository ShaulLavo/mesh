# Serve a dev server on demand

An on-demand route names the command behind a port. Mesh starts that command as
a session when the first connection arrives, and stops it once no connection has
been open for the idle window. Agents, browsers and scripts on the host all share
the one process, and nobody starts or stops it by hand.

```bash
mesh serve pc --run 'bun run dev:web' --cwd ~/projects/app \
  --listen 5173=15173 --listen 3001=13001 --idle 15m
```

Point the dev server at the upstream ports (15173 and 13001 here), bound to
`127.0.0.1`. Clients use the public ports (5173 and 3001). Mesh binds those on
`127.0.0.1` and proxies HTTP and WebSocket traffic from each one to its upstream.

## The route

| Flag | Meaning |
|---|---|
| `--run CMD` | The command. It runs through your login shell, so it finds the same tools a terminal would. |
| `--cwd DIR` | Where it starts. On this machine it defaults to the directory you ran `mesh serve` from. Another machine needs it named; a relative path there is under that user's home. |
| `--env KEY=VALUE` | Added to the daemon's environment. Repeatable. |
| `--listen PUBLIC=UPSTREAM` | A loopback listener on the host. Repeatable. |
| `--at /path` | Also serve the route on the tailnet at `/path`. Optional when `--listen` is given. |
| `--idle 15m` | How long the route runs with no open connection. |
| `--ready-timeout 60s` | How long a starting route holds connections before failing them. |

A route with no `--at`, or with `--at :5173`, is reached only through its
listeners. It is named by the listener port it is reached on, `:5173`: TARGET,
or the first `--listen` port when TARGET is left out. With `--at /dev`, the
route is `/dev`, and a request to `https://pc.mesh.shaulavo.dev/dev/` starts it
too. When TARGET is one of the listener ports, the tailnet path forwards
straight to that listener's upstream.

`--run` cannot be combined with `--public`. A route that starts a process when
someone on the internet opens a URL would be a different kind of exposure.

## What happens

- **First connection.** The daemon starts the command as a session labelled
  `serve /dev` (or `serve :5173`). `mesh ls` shows the label in the TITLE column,
  and `mesh ID` attaches to the session's output. Connections wait until every
  upstream port accepts.
- **Idle.** Every open connection counts: keep-alives, WebSockets, and requests
  on the tailnet path. When the count reaches zero the idle clock starts, and any
  new connection cancels it. When the clock runs out, the session is stopped the
  way `mesh kill` stops one. The listeners stay bound.
- **Failure.** If the command exits before its ports accept, or the ready timeout
  passes, the waiting connections get a 502. Its body names the route, the exit
  status, and the last lines of output. The route shows `failed` until the next
  connection tries again. Mesh does not restart it on its own.
- **Daemon restart.** The session keeps running. The new daemon binds the
  listeners again, adopts the session by its label, and starts a full idle
  window. After a reboot the next connection starts a fresh session.

A `--listen` port that something else already holds is refused, and the error
names the process holding it. So is one another route already listens on. Mesh
never picks another port.

Running `mesh serve` again with a new command, directory or environment stops
the running session; the next connection starts the new recipe. A new `--idle`
or `--ready-timeout` alone leaves it running.

## Commands

```bash
mesh serve ls                # STATE: stopped · starting · running (3 conns) · failed
mesh serve start :5173       # start now and wait until it is ready
mesh serve stop :5173        # stop now; the next connection starts it again
mesh unserve :5173           # stop it, release the ports, forget the route
```

A browser tab with a live HMR socket keeps the route running. The daemon closes
keep-alive connections that have been idle for two minutes, so a tab without a
live socket lets the route stop.
