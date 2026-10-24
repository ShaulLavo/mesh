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
Public hosting belongs to Brine on `shaulavo.dev`. See the
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
The listener guard and service registry use `serve.CanonicalHost`. DNS names
compare case-insensitively and may have a trailing root dot. An authority port
must be numeric and in range; it does not participate in Host identity.
Unknown authorities return 421 before routing or on-demand startup. Private
app forwarding uses the numeric origin endpoint as its wire Host and restores
the app's verified signed Host at the app handler.

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

Serving is Tailnet-only.

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

Arbitrary TCP tunnels, per-service authentication beyond Tailnet admission,
multi-user access control, the tidy `mesh.sprockt.dev/<name>` alias, and anything
resembling a build or deploy pipeline.
