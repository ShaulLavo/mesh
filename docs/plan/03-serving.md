# Step 8 — Serving

Mesh already knows which of your machines are up and how to reach them. Serving
adds one more verb: publish something from a machine, either to the tailnet or to
the public internet under your own domain.

Not a deployment system. Not a tunnel product. Three service types, one routing
table, and a single public entry point.

## What you can serve

- **static** — a directory, served as a website
- **files** — a directory, served as a browsable and downloadable listing
- **proxy** — a port already listening on that machine

## Where it lands

```
                    Internet
                       │
                       ▼
        mesh.shaul.dev  (A record → VPS public IP)
        VPS · mesh daemon in edge mode
          · terminates TLS
          · routes by path prefix
          · reverse-proxies over Tailscale
                       │
             ┌─────────┴──────────┐
             ▼                    ▼
      Desktop · daemon       Pi · daemon
      /blog  static          /files  files
      /api   proxy :3000
```

Public services are reachable at `mesh.shaul.dev/<name>`. Tailnet-only services
skip the VPS entirely and are reached straight from the origin machine, which is
the normal case and the default.

## The CLI

```bash
m serve pc ./site --at /blog             # tailnet only
m serve pc ./site --at /blog --public    # also at mesh.shaul.dev/blog
m serve pc 3000 --at /api --public       # proxy a local port
m serve pi /mnt/data --at /files --files
m serve ls
m unserve /blog
```

## Decisions

**The VPS is the public edge.** It already exists, it already has a public IP, and
it is already on the tailnet. Origin machines never open a port to the internet.

This is a scoped exception to "traffic goes directly to the destination machine".
That invariant is about terminal sessions, where a proxy would be a liability.
Public web traffic has to cross a public edge by definition. Terminal traffic
still never transits the VPS, and this exception does not extend to it.

**Tailnet-only by default.** Serving a directory is one keystroke from publishing
your home folder. `--public` is always deliberate.

**Path routing, with subdomains as the escape hatch.** `mesh.shaul.dev/blog` is
the default. A server that emits root-relative URLs will break under a path
prefix, so `--subdomain` publishes at `blog.mesh.shaul.dev` instead. Mesh
generates the HTML for `static` and `files`, so it handles its own prefixes
correctly; only `proxy` needs the escape hatch, and only when the proxied app
cannot be told its base path.

The certificate covers `mesh.shaul.dev` and `*.mesh.shaul.dev`, which needs a
DNS-01 challenge. Doing this once at the start is much less painful than
retrofitting the wildcard later.

**Origins register with the edge.** The edge cannot fan out to every host on each
request, so origin daemons push their public services to it and refresh on a
heartbeat. The edge keeps the routing table in its own SQLite and only accepts
registrations from host identities it knows (T06).

**An offline origin returns 502, honestly.** No silent staleness. A public service
backed by the desktop can opt into `--wake-on-request`, which uses the Pi's
wake-on-LAN from step 6 and holds the request while the machine comes up. That is
the payoff for having built waking already.

## Tasks

- `T11-serving-core.md` — service types, registry, origin-side serving
- `T12-public-edge.md` — VPS edge mode, TLS, routing, registration
- `T13-serve-cli.md` — `m serve`, `m unserve`, `m serve ls`

## Explicitly not in step 8

Arbitrary TCP tunnels, per-service auth beyond public or tailnet, multi-user
access control, custom domains other than `mesh.shaul.dev`, and anything
resembling a build or deploy pipeline.
