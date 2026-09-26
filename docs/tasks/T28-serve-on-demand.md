# T28 — Serve a session on demand

**Status:** proposed 2026-09-26 · **Prerequisites:** T11, T14, T27

## Outcome

A proxy route can name the command behind its port. Mesh starts that command
as a session when the first connection arrives, and stops it when no connection
has been open for the idle window. Every client on the host — agents, browsers,
scripts — shares the one process, and nobody starts or stops it by hand.

The case that prompted this is a development server. Several agent sessions and
a browser work against one Vite and API pair on fixed ports. Keeping it running
permanently holds memory all day (Vite's grows under load). Letting each session
start its own gives port races, duplicate servers, and a server nobody owns once
its session ends.

## Why this is still serving (D22)

D22 limits serving to publishing what already runs on your own machine. T28
keeps that line. It builds nothing, versions nothing, and hands nothing to
anyone. The process is an ordinary Mesh session with a launch recipe, and the
route only decides *when* it runs. That is T27's hibernation with a different
wake signal: a connection instead of an attach, and connection idleness instead
of output idleness.

## Interface

```bash
mesh serve pc 5173 --at /dev \
  --run 'bun run dev:web' --cwd ~/projects/app \
  --listen 5173=15173 --listen 3001=13001 \
  --idle 15m
mesh serve ls                  # STATE: stopped · starting · running (3 conns) · failed
mesh serve start /dev          # start now, e.g. to warm it
mesh serve stop /dev           # stop now; the next connection starts it again
mesh unserve /dev              # stop, release the ports, forget the route
```

- `--run` makes the route on-demand. `--cwd` defaults to the directory
  `mesh serve` was run from, and the environment is the daemon's plus
  `--env KEY=VALUE` flags. The recipe is stored with the route.
- `--listen PUBLIC=UPSTREAM` (repeatable) binds `127.0.0.1:PUBLIC` on the
  origin and proxies it to `127.0.0.1:UPSTREAM`. It is how local clients reach
  the route without going through the tailnet name. `--at` becomes optional
  when `--listen` is present. It stays HTTP, WebSocket upgrades included; this
  is not the arbitrary TCP tunnel step 8 rules out.
- `--idle` defaults to 15 minutes. `--ready-timeout` (default 60 s) bounds how
  long a starting route holds connections.

## Design

- **One session per route.** On the first connection to any of the route's
  listeners or its tailnet path, the daemon creates a session from the recipe,
  with a label naming the route so `mesh ls` shows it and `mesh ID` attaches to
  its output. Connections wait while every upstream port refuses, up to
  `--ready-timeout`. Once all of them accept, the waiting connections go
  through.
- **Idleness is open connections.** The proxy counts live connections across
  all listeners and the tailnet path: HTTP keep-alives and upgraded WebSockets
  each count until closed. When the count reaches zero, the idle clock starts;
  any new connection cancels it. When it expires, the daemon stops the session
  the way `mesh kill` does (SIGHUP to the group, then SIGKILL after the grace).
  The listeners stay bound.
- **The daemon owns the listeners, the worker owns the process.** Invariant 3
  holds: a daemon restart re-binds the listeners, re-adopts the live session
  by its route label, and restarts the idle clock at a full window. A reboot
  leaves the session `interrupted` (invariant 5), and the next connection
  starts a fresh one from the recipe. That is an honest new start, not a
  resurrection.
- **Failure is visible.** If the command exits before its ports accept, or the
  ready timeout passes, waiting connections get a 502 whose body names the
  route, the exit status, and the last lines of the session's output. The route
  shows `failed` until the next connection retries. There is no automatic
  restart loop: a crash while serving drops its connections, and the next one
  starts it again.
- **Ports are claimed loudly.** `mesh serve` refuses a `--listen` port that is
  already held, and names the holding process. It never picks another port.
- **Health** keeps T14's meaning for a running route. A stopped on-demand route
  is `stopped`, not `unhealthy`.

## Open questions

- **Q1 — tailnet path.** Should an on-demand route also accept its tailnet `--at`
  path when idle, starting the process from another machine's request?
  Recommend yes: it's the same route, and T19 already waits for a whole host.
- **Q2 — explicit holds.** Should a session be able to hold the route without a
  connection (`mesh serve hold /dev` for the length of a job)? Recommend not
  in T28; connections have covered every case so far.

## Verification

An integration script, `integration/serve_on_demand.sh`, with a fake server that
binds two ports after a delay:

- The first request starts the session, waits for readiness, and succeeds;
  `mesh ls` shows the labelled session.
- Two clients, one closes: the session stays. Both close: after the idle window
  the session has exited and both listeners still accept.
- A WebSocket held open keeps the route running past the idle window.
- A command that exits at once gives a 502 naming its exit status and output,
  and `mesh serve ls` shows `failed`.
- Restarting the daemon mid-run keeps the session running; clients reconnect
  through the re-bound listeners, and the idle clock starts over.
- `--listen` on a held port is refused with the holder named.
