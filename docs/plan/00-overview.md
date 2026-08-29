# Mesh v0 — overview

Mesh is a Go application that gives you direct, resumable terminal sessions across
your Tailscale-connected computers.

It is not a VPN, scheduler, coding agent, MCP platform, deployment system, or
personal assistant — yet. Those can consume Mesh later.

```mermaid
flowchart TD
    M["MacBook · Mesh CLI"] -->|"WebSocket over Tailscale"| PC["Desktop · mesh daemon"]
    M -->|"WebSocket over Tailscale"| PI["Pi · mesh daemon"]
    M -->|"WebSocket over Tailscale"| VPS["VPS · mesh daemon"]

    PC --> PW["Persistent PTY workers"]
    PI --> WOL["Wake desktop"]
    VPS --> DP["Deployment shell"]

    SSH["SSH"] -. "Install / recovery" .-> PC
```

## The contract

Non-negotiable invariants, repeated in `CLAUDE.md` because they are the product:

- Connections are disposable.
- Sessions belong to the remote host, not the client connection.
- Losing wifi, closing the MacBook, or killing `mesh` must not kill the command.
- Traffic goes directly to the destination machine.
- The Pi helps with waking and discovery; it does not proxy terminal traffic.
- Tailscale handles networking and reachability. Mesh handles sessions,
  reconnection, history, and UX.
- Rebooting a host kills its processes. Mesh reports those sessions as
  `interrupted`.

## CLI

```bash
alias m=mesh

m pc                 # always create a new session
m pc -r              # resume latest active session on pc
m 7K3D               # attach exact session
m                    # interactive host/session picker
m ls                 # sessions across every known host
m logs 7K3D
m kill 7K3D
m wake pc

mesh add shaul@pc    # SSH bootstrap, once per machine
```

`mesh add` uses SSH to install and start Mesh, records the stable Mesh identity,
discovers its Tailscale address, and verifies the WebSocket connection. After
that, SSH disappears behind the curtain.

## Runtime design

Every machine runs `mesh daemon`. When the daemon creates a terminal session it
launches a detached `mesh session-worker` process, which:

- owns exactly one PTY;
- owns the shell / Codex / Claude process;
- survives the CLI disconnecting;
- survives the main daemon crashing and restarting;
- maintains output sequence numbers;
- stores bounded scrollback;
- exposes a local Unix socket;
- maintains terminal state for clean reattachment.

The daemon is a coordinator. It rediscovers existing workers after a restart and
connects remote WebSocket clients to them. If the daemon owned every PTY, a daemon
update would nuke live work — the precise clownery Mesh exists to prevent.

## Transport

WebSockets over Tailscale.

- One WebSocket per client-to-host connection; sessions multiplexed over it.
- JSON control messages, binary frames for terminal data.
- Compression disabled. Output batched.
- Ping/pong for health; reconnect with backoff.
- Local daemon-to-worker communication over Unix sockets, same framing.

On reconnect the client sends its last acknowledged sequence. The host returns
missed output, or a fresh terminal snapshot if the replay window has expired.

## Session data

SQLite stores metadata, never the live process:

```
id · host_id · command · cwd · state · created_at · last_attached_at ·
exit_code · last_output_sequence
```

States: `running`, `detached`, `exited`, `interrupted`.

`m ls` queries every known online host concurrently, merges results, falls back to
a local cache for offline hosts, and marks cached rows as stale. The Pi may cache
summaries later; it never owns another machine's sessions.

## Stack

Core: Go 1.27 · Cobra · Fang · `coder/websocket` · `modernc.org/sqlite` · sqlc ·
Goose · `golang.org/x/crypto/ssh`.

Charm: Bubble Tea v2 · Bubbles v2 · Lip Gloss v2 · Huh v2 · Glamour v2 ·
Charm Log v2 · Wish v2 · Harmonica · `x/xpty` · `x/vt` · `x/ansi` · `x/term` ·
`x/input` · `x/sshkey` · `x/teatest` · `x/golden` · `x/vcr`.

Pin the experimental packages to exact versions and hide them behind internal
interfaces so API churn stays contained.

## Build order

1. **Local persistent session** — daemon, detached worker, PTY, Unix socket
   attach, detach/reattach, resize, signals, bounded output. *This is the heart.*
2. **Remote session over Tailscale** — WebSocket server, binary streaming, control
   protocol, reconnect/backoff, sequence acknowledgement, host identity, Tailscale
   address discovery.
3. **Product CLI** — host aliases, `m pc`, `-r`, `m 7K3D`, `ls`, `logs`, `kill`,
   interactive picker.
4. **SSH bootstrap** — `mesh add`, OS/arch detection, install, service unit,
   identity, host record, connection test, useful diagnostics.
5. **Crash and restart recovery** — every disconnection mode, worker rediscovery,
   `interrupted` marking.
6. **Pi power control** — `m wake pc`, wake-then-connect flow, sleep inhibitors.
7. **Packaging** — systemd, launchd, GoReleaser, Actions, Homebrew Cask, installer,
   VHS demos, race tests, protocol fuzzing, govulncheck, golangci-lint v2.

## Explicitly later

MCP/tools · coding-agent knowledge · job scheduling · deployment orchestration ·
personal assistant · iPhone app · GUI editor · collaborative writers · sharing
meshes with other users · replacing Tailscale · arbitrary proxying through the Pi.

The extension point is the daemon protocol. Anything built later calls Mesh
exactly like the CLI does.
