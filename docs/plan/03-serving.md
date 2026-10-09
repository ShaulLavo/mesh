# Step 8 — Private serving

Status: Approved, updated for the 2026-10-08 public-feature removal.

Mesh serves static directories, file listings and local-port proxies on the
Tailnet. Private machine names use `<host>.mesh.sprockt.dev`; configured short
service names, such as `fregat.sprockt.dev`, serve at the hostname root.
The owner’s private services are `https://fregat.sprockt.dev/`,
`https://ai.sprockt.dev/` and `https://comfy.sprockt.dev/`. Their old machine-path
aliases are retired.

The Approved [temporary-app plan](06-temporary-apps.md) adds a separate
`mesh app` capability for private disposable websites. D30 narrows
the older D22 boundary without changing ordinary serving behavior.

Public temporary apps, public serving and VPS-edge reverse tunnels are removed.
Public hosting belongs to Brine on `shaulavo.dev`. Public-edge sections below
record the former implementation and are superseded by this decision; they are
not current deployment instructions or optional Mesh features. See the
[superseding decision note](01-decisions.md).

## Private names and access

| Name | Audience | Routing |
|---|---|---|
| `<host>.mesh.sprockt.dev` | Tailnet devices | Direct to the host; services use declared paths |
| A configured service name such as `fregat.sprockt.dev` | Tailnet devices | The service uses the hostname root |
| `<id>.sprockt.dev` | Authorized Tailnet app viewers | Private app ingress and registry admission |

Names resolve to private Tailnet ingress. Publicly trusted HTTPS certificates
provide browser trust; they do not grant internet access. Private machine Hosts
use longest-prefix route matching. Configured short service names use only their
hostname root; retired machine-path aliases do not expose those services.

Private service requests share a browser-request policy with private apps.
An `Origin` different from the request's own origin returns 403 before any
handler runs, including on-demand startup and mount redirects. Mesh derives
that origin from the connection's TLS state and Host, not forwarded headers.
There is one exception for non-WebSocket requests: `Origin: null` is allowed
with `Sec-Fetch-Site: same-origin`. Browsers can send that combination on a
same-origin form POST under `Referrer-Policy: no-referrer`. A page cannot forge
Fetch Metadata. Null Origin remains refused with cross-site, same-site, or
missing Fetch Metadata, and on every WebSocket upgrade.

A WebSocket upgrade with `Origin` requires an exact match. Without `Origin`,
private services admit native WebSocket clients, because browsers send that
header on upgrades. Browser-facing private apps still require `Origin`, as
established in PR #7. The shared helper takes an explicit WebSocket-origin
policy to preserve that difference. Other requests allow
`Sec-Fetch-Site: same-origin` or `none`, or a top-level document navigation with
GET or HEAD. Where browsers send Fetch Metadata, including private HTTPS and
loopback HTTP, the gate refuses same-site sibling fetches, cross-site fetches,
frames, and unknown Fetch Metadata values unless they are top-level GET/HEAD
visits. A refused service request returns `cross-site request to private service`.
The former public-Host exception is historical and superseded by the
2026-10-08 public-feature removal.

Browsers generally omit Fetch Metadata on ordinary plaintext tailnet HTTP
such as `http://100.x:7337` or `http://pc:7337`. Without those headers, every
method remains allowed when `Origin` is absent or matches, preserving native
clients. Unsafe browser requests rely on Origin checks; headerless GET
subresources can still reach services and trigger on-demand startup.
No-referrer forms with null Origin and no Fetch Metadata remain refused.
Use the private HTTPS name for browser apps that need the full metadata gate.

This policy is not authentication for clients already admitted to the tailnet.
Services under one private host share one origin, so it does not isolate one
route from another. Cross-origin consumers, including a localhost Vite page
fetching a private service, are refused when they send a foreign Origin;
a configurable exception is a follow-up, not supported here. Headerless HTTP
clients and top-level GET navigation remain allowed, so upstream applications
must keep GET read-only and protect their own mutations.

Certificates work anyway. Let's Encrypt DNS-01 validates by publishing a TXT
record, never by connecting to the host, so `*.mesh.sprockt.dev` gets a real
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
output cannot forge log entries. The synchronous log write completes before any
waiter receives the reference, bypassing the daemon's best-effort error queue.
Admission errors, including cancellation and removed routes, use the same log
path with their own references. If the log write fails, the generic response
omits the reference rather than claiming a diagnostic was recorded.
On Linux, read the daemon log with
`journalctl --user -u mesh`. On macOS, it is
`~/.local/state/mesh/daemon.err.log`.
`mesh serve ls` reports the failed state and session ID. `mesh logs SESSION`
retains the process output.

## Where it lands

A Tailnet device opens a private HTTPS name. Machine service traffic reaches the
origin host, which serves a directory, file listing or loopback proxy. Configured
short service names select the service at its hostname root. Private app ingress
checks Tailnet identity and app admission before forwarding to its owner origin.
Terminal traffic remains direct between hosts.

## Reaching the private name

Two ways to make `mesh.sprockt.dev` resolve, and they can coexist:

**Per-host, direct.** `pc.mesh.sprockt.dev` and `pi.mesh.sprockt.dev` are A records
holding each machine's tailnet address. Services appear at
`pc.mesh.sprockt.dev/blog`. Nothing proxies, nothing is a single point of failure,
and this keeps the "traffic goes directly to the destination machine" invariant
completely intact. This is the default.

**Tidy alias.** `mesh.sprockt.dev/blog` with no machine in the URL requires
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
- Certificate-backed private names published by
  the certificate runtime. Each becomes accepted only after ingress
  is ready and stops being accepted if the source withdraws it. Additional
  labels below that name are not aliases: neither the private certificate nor
  the managed DNS records cover them. Machine names route services by path;
  configured short service names use their hostname root.
- Historical, superseded 2026-10-08: a canonical one-label public name accepted by `serve.ValidatePublicName`,
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

Historical public-edge behavior (superseded 2026-10-08): public-name traffic was refused until a
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
m serve pc ./site --at /blog                # pc.mesh.sprockt.dev/blog
m serve pc 3000 --at /api                   # proxy a local port, tailnet only
m serve pc ./app --at /app --isolate        # add COOP/COEP so the page gets SharedArrayBuffer
m serve pi /mnt/data --at /files --files
m serve ls
m unserve /blog
```

Serving is Tailnet-only. The former `--public` workflow is superseded.

## Decisions

Serving and temporary apps are Tailnet-only. Public hosting belongs to Brine on
`shaulavo.dev`. Former public-edge design decisions are preserved in
[the decision history](01-decisions.md) and
[the superseded T13 specification](../tasks/T13-public-edge.md).

## Tasks

- `T11-serving-core.md` — service types, registry, origin-side serving
- `T12-private-names.md` — DNS and TLS for `mesh.sprockt.dev`, per-host routing
- `T13-public-edge.md` — historical VPS edge spec, superseded 2026-10-08
- `T14-serve-cli.md` — `m serve`, `m unserve`, `m serve ls`
- `T28-serve-on-demand.md` — a proxy route that starts its command on the first
  connection and stops it when idle

## Explicitly not in step 8

Arbitrary TCP tunnels, per-service authentication beyond public or tailnet,
multi-user access control, the tidy `mesh.sprockt.dev/<name>` alias, and anything
resembling a build or deploy pipeline.

## The apex is not Mesh's — historical evidence, 2026-08-29

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

## Public connection limits — historical, superseded 2026-10-08

The public listener admits at most 512 connections. Each direct IPv4 peer or
IPv6 /64 may hold at most 32, one sixteenth of that pool. Authenticated PROXY
sources use the same quota. HTTP forwarding headers never establish a
connection identity. Without PROXY, a loopback peer represents the local front
door rather than one visitor, so it has no per-source quota. Caddy, nginx, or
another such front door must enforce per-client connection limits itself.
Its upstream sockets still share Mesh's global cap.

PROXY authentication has a separate pool of 32 pending sockets and a fixed
two-second deadline. Local forwarders send their headers immediately. A full
pending pool waits rather than bypassing authentication. Invalid headers and
disallowed forwarder UIDs close without consuming an admitted slot or evicting
an authenticated connection.

HTTP headers have five seconds and idle keep-alives have 30 seconds. At the
global cap, an under-quota newcomer evicts the oldest idle keep-alive. Active
responses and hijacked WebSockets are never evicted. If no idle socket exists,
Mesh closes the newcomer instead of waiting for an active slot.

Request bodies get a 30-second idle allowance and a sustained minimum of
16 KiB/s, measured only during body reads. Origin wake-up, on-demand startup,
and server pauses between reads do not charge the client's budget. Once a
response starts or upgrades, body deadlines are cleared. HTTP/1 enables
full-duplex handling so an early response is not blocked by implicit body
draining. After a non-hijacked handler finishes, the final body drain is bounded
again. Very slow client uploads below the minimum eventually time out.

These limits are not a distributed denial-of-service defense. Sixteen IPv4
addresses, or sixteen /64s from one wider IPv6 allocation, can still fill the
pool with active responses or half-open connections. There is no wider-prefix
aggregate quota or response write-progress timeout. A front door must provide
those additional abuse controls when needed.

## The browser is not the only file client

`files` services render HTML listings, which exist for people holding a browser
and nothing else. Step 9 mounts the same declared roots over SFTP, where they
open in Finder, Nautilus and Files on Android with keys already on the machine
(D19).

One declaration, two front doors. Nothing here changes: T11 still owns what is
served and to whom, and T16 reuses its root resolver rather than inventing a
second answer. See `docs/plan/04-ssh.md`.
