# Step 8 — Serving

Mesh already knows which of your machines are up and how to reach them. Serving
adds one verb: publish something from a machine, privately on the tailnet or
publicly on the internet.

This plan covers ordinary serving: three service types and two front doors.
The Approved [temporary-app plan](06-temporary-apps.md) adds a separate
`mesh app` capability for disposable websites and public sharing. D30 narrows
the older D22 boundary without changing ordinary serving behavior.

Publishing here is rarer than it sounds. The common case is a directory or a
local port you want to reach from your phone on the tailnet. Reaching the public
internet at all is the exception, which is why D15 makes you name it.

Public exposure stays in the plan. It is worth having for the occasional app on
the desktop that someone outside the tailnet needs to hit, and DNS-01 is already
built for the private side, so the marginal cost is the edge itself. Being rare
is an argument for keeping that edge small, not for dropping it.

## Two names, two audiences

| | `<host>.mesh.shaulavo.dev` | `<name>.shaulavo.dev` |
|---|---|---|
| Audience | tailnet only | the internet |
| Resolves to | tailnet `100.x` addresses | the VPS public IP |
| Reached by | direct, host to host | through the VPS edge |
| Default | yes | needs `--public` |

`mesh.shaulavo.dev` is the private namespace. Its per-host DNS records point at
tailnet addresses, so they resolve to something reachable only from the
tailnet. Everyone else gets an address they cannot route to.

For a canonical public Host, the origin serves only the longest matching route
published under that Host. A nested private route or a route published under
another hostname returns 404 before proxying or starting an on-demand process,
without falling back to a public parent. Private tailnet Hosts keep the existing
longest-prefix routing, including requests from the public edge host.

Certificates work anyway. Let's Encrypt DNS-01 validates by publishing a TXT
record, never by connecting to the host, so `*.mesh.shaulavo.dev` gets a real
publicly trusted certificate despite the per-host records pointing at
unroutable addresses. No custom CA, no browser warnings, no per-device trust
store surgery.

## What you can serve

- **static** — a directory, served as a website
- **files** — a directory, served as a browsable and downloadable listing
- **proxy** — a port already listening on that machine

On-demand startup failures return a generic HTTP message, an opaque reference,
and an owner hint to use `mesh serve ls` and `mesh logs <session>` on the host.
All requests waiting on one failed start share its reference. The daemon logs
that start once, with the command, session ID, and output tail quoted so process
output cannot forge log entries. On Linux, read the daemon log with
`journalctl --user -u mesh`. On macOS, it is
`~/.local/state/mesh/daemon.err.log`.
`mesh serve ls` reports the failed state and session ID. `mesh logs SESSION`
retains the process output.

## Where it lands

```
Tailnet                                    Internet
   │                                          │
   ▼                                          ▼
pc.mesh.shaulavo.dev                 blog.shaulavo.dev
(A → tailnet addresses)     (A → VPS public IP)
   │                                          │
   │  direct, no proxy                        ▼
   │                              VPS · TLS front door
   │                                  · mesh daemon, edge mode
   │                                  · routes to origins
   │                                          │
   └──────────────┬───────────────────────────┘
                  ▼
        Desktop · daemon        Pi · daemon
        /blog  static           /files  files
        /api   proxy :3000
```

## Reaching the private name

Two ways to make `mesh.shaulavo.dev` resolve, and they can coexist:

**Per-host, direct.** `pc.mesh.shaulavo.dev` and `pi.mesh.shaulavo.dev` are A records
holding each machine's tailnet address. Services appear at
`pc.mesh.shaulavo.dev/blog`. Nothing proxies, nothing is a single point of failure,
and this keeps the "traffic goes directly to the destination machine" invariant
completely intact. This is the default.

**Tidy alias.** `mesh.shaulavo.dev/blog` with no machine in the URL requires
something always-on to route by path, which means the Pi. That makes the Pi a
tailnet web router, and if the Pi is down every tidy URL is down while every
per-host URL keeps working.

Build the per-host form first, because it is strictly simpler and cannot fail
partially. Add the alias afterwards if typing the machine name actually annoys
you, which it might not.

## Private listener Host policy

The tailnet HTTP listener and loopback HTTPS listener check `Host` before
routing any non-control request to an app, file listing, proxy, or on-demand
service. An unrecognized Host returns `421 Misdirected Request` without
contacting an upstream or starting a command. Being on the tailnet alone does
not authorize a browser request under an attacker-controlled DNS name.

Accepted authorities are:

- An IPv4 or IPv6 literal equal to an address whose tailnet HTTP listener
  successfully bound. IPv4-mapped IPv6 literals compare as IPv4.
- `localhost` and loopback literals (`127.0.0.0/8` and `::1`), including
  IPv4-mapped loopback. These preserve SSH forwards such as
  `ssh -L 12000:<tailnet-ip>:7337`, then `http://localhost:12000/<service>`.
  The forwarding authority need not be the origin listener's address. An
  attacker-controlled DNS name still does not become a loopback Host.
- This host's full MagicDNS name and its short first label, obtained from local
  Tailscale discovery. Other hosts on the same tailnet are not aliases. The
  short name is the only accepted name whose safety depends on the client's
  resolver using MagicDNS rather than a hostile LAN resolver. These names are
  captured at startup; a MagicDNS rename needs a daemon restart to take effect.
- Exactly the current certificate-backed private name published by
  `certificateRuntime.PrivateName()`. It becomes accepted only after ingress
  is ready and stops being accepted if the source withdraws it. Additional
  labels below that name are not aliases: neither the private certificate nor
  the managed DNS records cover them. Private services route by path.
- A canonical one-label public name accepted by `serve.ValidatePublicName`,
  only when the immediate TCP peer matches the identity-verified public edge's
  pinned address through `trustPublicEdgeForwarding`. Forwarding headers do not
  establish trust. This admits a public authority; the downstream dispatcher
  still owns which route that authority may serve.

The listener guard and service registry use the same `serve.CanonicalHost`
helper: DNS names compare case-insensitively and may have a trailing root dot.
Public-name variants therefore remain public during route selection and cannot
fall through to private nested routes. An optional authority port must be
numeric and in range, but does not participate
in the Host identity decision. Tailscale Serve, the public edge, and port
forwards can preserve an external authority port that differs from the internal
listener. IPv6 zones, malformed authorities, the public apex, and nested public
names are refused.

Public-name traffic, including app-origin requests, is refused until a
successful edge publication establishes the pin. After a failed initial sync,
the next scheduled attempt is a minute later. The connection's immediate
source address must match that pin; both edge resolvers currently prefer IPv4.
The loopback integration fixtures do not prove a deployed address-family match.

Service WebSocket upgrades follow the same Host policy as service HTTP. The
WebSocket control path retains its separate browser Origin refusal. The
loopback HTTPS listener still returns 404 for that path. IP-dialed CLI and
host-to-host control connections do not change. Accepting a loopback Host does
not add a loopback name to the private TLS certificate; the SSH-forward example
uses HTTP.

## The CLI

```bash
m serve pc ./site --at /blog                # pc.mesh.shaulavo.dev/blog
m serve pc 3000 --at /api                   # proxy a local port, tailnet only
m serve pc ./app --at /app --isolate        # add COOP/COEP so the page gets SharedArrayBuffer
m serve pi /mnt/data --at /files --files
m serve pc ./site --at /blog --public blog.shaulavo.dev
m serve ls
m unserve /blog
```

Private is the default and needs no extra words. Public names are typed out in
full, every time, because that is the irreversible one.

## Decisions

**The VPS is the public edge, and nothing else is.** It already has a public IP
and is already on the tailnet. Origin machines never open a port to the internet.

This is a scoped exception to "traffic goes directly to the destination machine".
That invariant protects terminal sessions, where a proxy would be a liability.
Public web traffic crosses a public edge by definition. Terminal traffic still
never transits the VPS.

**Mesh never claims the `shaulavo.dev` apex.** That is your site, not Mesh's. Every
public route is named explicitly per service. Mesh will refuse to bind a public
name it was not told to bind. D16 narrows D15's explicit-name rule: typing the
apex does not authorize Mesh to serve it.

**Tailnet-only by default.** Serving a directory is one keystroke from publishing
your home folder.

**Wildcard certificates from day one.** `*.mesh.shaulavo.dev` needs DNS-01
regardless. The optional direct-TLS edge also uses DNS-01 for
`*.shaulavo.dev`. The two certificate profiles use separate state and cannot
install into each other's serving slot.

**An offline origin returns 502, honestly.** No silent staleness. T13 keeps
`--wake-on-request` behind a bounded interface. T19 supplies the wake client:
the target must allow wake, and a sender must be awake on its LAN. The edge
waits up to 90 seconds for the target and a fresh service publication.

## Tasks

- `T11-serving-core.md` — service types, registry, origin-side serving
- `T12-private-names.md` — DNS and TLS for `mesh.shaulavo.dev`, per-host routing
- `T13-public-edge.md` — VPS edge mode, public routes on `shaulavo.dev`
- `T14-serve-cli.md` — `m serve`, `m unserve`, `m serve ls`
- `T28-serve-on-demand.md` — a proxy route that starts its command on the first
  connection and stops it when idle

## Explicitly not in step 8

Arbitrary TCP tunnels, per-service authentication beyond public or tailnet,
multi-user access control, the tidy `mesh.shaulavo.dev/<name>` alias, and anything
resembling a build or deploy pipeline.

## The apex is not Mesh's

`shaulavo.dev` serves nothing today. When the site arrives it will not be served
by Mesh, so the edge is designed to sit behind the front door rather than be it.

The practical consequence: **the Mesh edge must not assume it owns port 443.**
Whatever serves the site owns it, terminates TLS, and reverse-proxies the Mesh
routes to the edge on a local port. The edge respects `X-Forwarded-Proto` so
generated links stay `https`.

That also means TLS for public names may not be Mesh's job at all. If Caddy or
nginx fronts the VPS, it already holds those certificates and the edge just
speaks plain HTTP on localhost. The DNS-01 plumbing does not disappear, because
the private `*.mesh.shaulavo.dev` wildcard still needs it (T12), but T13 gets
noticeably smaller.

Where the site will live is undecided as of 2026-08-29. The candidates map onto
the two arrangements cleanly, so T13 is not blocked on choosing:

| Host | Where the site runs | What the Mesh edge does |
|---|---|---|
| Coolify | on the VPS | plain HTTP behind Coolify's Traefik, which owns 443 |
| Cloudflare Workers | off the VPS | edge can own 443 |
| Vercel | off the VPS | edge can own 443 |

One practical note if it turns out to be Coolify: it brings Docker, Postgres,
Redis and Traefik onto the same VPS that runs the Mesh daemon and whatever
terminal sessions are attached to it. Check the box has headroom before
committing, because a session dying under memory pressure is exactly the failure
Mesh exists to prevent.

If it turns out to be Cloudflare, the public Mesh routes could sit behind
Cloudflare too, which hides the VPS address and absorbs abuse. The tradeoff is
that Cloudflare terminates TLS and therefore sees that traffic. Worth deciding
deliberately rather than by default.

## The browser is not the only file client

`files` services render HTML listings, which exist for people holding a browser
and nothing else. Step 9 mounts the same declared roots over SFTP, where they
open in Finder, Nautilus and Files on Android with keys already on the machine
(D19).

One declaration, two front doors. Nothing here changes: T11 still owns what is
served and to whom, and T16 reuses its root resolver rather than inventing a
second answer. See `docs/plan/04-ssh.md`.
