# Step 8 — Serving

Mesh already knows which of your machines are up and how to reach them. Serving
adds one verb: publish something from a machine, privately on the tailnet or
publicly on the internet.

Not a deployment system. Not a tunnel product. Three service types, two front
doors.

## Two names, two audiences

| | `mesh.shaulavo.dev` | `shaulavo.dev` |
|---|---|---|
| Audience | tailnet only | the internet |
| Resolves to | tailnet `100.x` addresses | the VPS public IP |
| Reached by | direct, host to host | through the VPS edge |
| Default | yes | needs `--public` |

`mesh.shaulavo.dev` is private. Its DNS records point at tailnet addresses, so it
only resolves to anything reachable if you are on the tailnet. Everyone else gets
an address they cannot route to.

Certificates work anyway. Let's Encrypt DNS-01 validates by publishing a TXT
record, never by connecting to the host, so `mesh.shaulavo.dev` gets a real
publicly-trusted certificate despite pointing at unroutable addresses. No custom
CA, no browser warnings, no per-device trust store surgery.

## What you can serve

- **static** — a directory, served as a website
- **files** — a directory, served as a browsable and downloadable listing
- **proxy** — a port already listening on that machine

## Where it lands

```
Tailnet                                    Internet
   │                                          │
   ▼                                          ▼
mesh.shaulavo.dev              shaulavo.dev / blog.shaulavo.dev
(A → tailnet addresses)     (A → VPS public IP)
   │                                          │
   │  direct, no proxy                        ▼
   │                              VPS · mesh daemon, edge mode
   │                                · terminates TLS
   │                                · routes to origins
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

## The CLI

```bash
m serve pc ./site --at /blog                # pc.mesh.shaulavo.dev/blog
m serve pc 3000 --at /api                   # proxy a local port, tailnet only
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
name it was not told to bind.

**Tailnet-only by default.** Serving a directory is one keystroke from publishing
your home folder.

**Wildcard certificates from day one.** `*.mesh.shaulavo.dev` needs DNS-01 regardless,
so build the DNS-01 plumbing first rather than starting with HTTP-01 and
retrofitting. One provider credential, stored like any other secret.

**An offline origin returns 502, honestly.** No silent staleness. A public service
backed by the desktop can opt into `--wake-on-request`, which uses the Pi's
wake-on-LAN from step 6 and holds the request with a bounded deadline. That is
the payoff for having already built waking.

## Tasks

- `T11-serving-core.md` — service types, registry, origin-side serving
- `T12-private-names.md` — DNS and TLS for `mesh.shaulavo.dev`, per-host routing
- `T13-public-edge.md` — VPS edge mode, public routes on `shaulavo.dev`
- `T14-serve-cli.md` — `m serve`, `m unserve`, `m serve ls`

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

## Tasks

- `T11-serving-core.md` — service types, registry, origin-side serving
- `T12-private-names.md` — DNS and TLS for `mesh.shaulavo.dev`, per-host routing
- `T13-public-edge.md` — VPS edge mode, public routes on `shaulavo.dev`
- `T14-serve-cli.md` — `m serve`, `m unserve`, `m serve ls`

## Explicitly not in step 8

Arbitrary TCP tunnels, per-service authentication beyond public or tailnet,
multi-user access control, the tidy `mesh.shaulavo.dev/<name>` alias, and anything
resembling a build or deploy pipeline.

## The apex

`shaulavo.dev` serves nothing today, but it will. When the personal site arrives,
the simplest thing is for it to be a Mesh service like any other:

```bash
m serve vps ./site --public shaulavo.dev
```

One TLS terminator, one routing table, and the site gets the same restart and
health behaviour as everything else. No coexistence problem, because there is
nothing to coexist with.

The alternative is running Caddy or nginx on the VPS and putting the Mesh edge
behind it. That is a real option if the site outgrows static files, so the edge
must be able to run on a port other than 443 and behind a proxy that terminates
TLS for it. Support that from the start. It costs a configuration flag now and a
rewrite later.

What the edge must never do is assume it owns port 443 by right.

## The browser is not the only file client

`files` services render HTML listings, which exist for people holding a browser
and nothing else. Step 9 mounts the same declared roots over SFTP, where they
open in Finder, Nautilus and Files on Android with keys already on the machine
(D19).

One declaration, two front doors. Nothing here changes: T11 still owns what is
served and to whom, and T16 reuses its root resolver rather than inventing a
second answer. See `docs/plan/04-ssh.md`.
