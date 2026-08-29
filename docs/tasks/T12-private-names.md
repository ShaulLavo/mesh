# T12 — Private names and certificates for `mesh.shaul.dev`

**Status:** not started · **Blocked by:** T11, T06 · **Owns:** `internal/dnsname/`

## Goal

`https://pc.mesh.shaul.dev/blog` works from any device on the tailnet, with a
valid certificate and no browser warning, and resolves to nothing routable for
anyone else.

## Why this is possible

`mesh.shaul.dev` records hold tailnet `100.x` addresses. They are published in
ordinary public DNS, so every resolver answers, but the answer is only reachable
from the tailnet.

Certificates come from Let's Encrypt via DNS-01, which validates by publishing a
TXT record and never connects to the host. A name that resolves to an unroutable
address gets a real publicly-trusted certificate. This is the whole trick, and it
is why no custom CA or per-device trust configuration is needed.

## Responsibilities

1. **DNS records.** Maintain an A record per host: `pc.mesh.shaul.dev`,
   `pi.mesh.shaul.dev`, and so on, pointing at each machine's current tailnet
   address. Tailnet addresses are stable in practice but not guaranteed, so
   reconcile them rather than setting them once by hand.
2. **Certificates.** A wildcard for `*.mesh.shaul.dev` via DNS-01, renewed
   unattended, distributed to each host's daemon. Decide and document where the
   private key lives and which machine performs renewal.
3. **Serving.** Each origin daemon terminates TLS for its own name and serves its
   own services. No proxy, no router, no shared front door.

## Decisions to make and record

- **Which DNS provider and credential.** DNS-01 needs an API token that can write
  TXT records for `shaul.dev`. That token is the most sensitive thing in this
  task. Scope it to the zone, store it on one machine, and say which.
- **Who renews.** One machine renewing and distributing is simpler than every
  machine holding the zone credential. The Pi is the obvious candidate since it
  is always on.

## The tradeoff to state plainly

Publishing tailnet addresses in public DNS makes them visible to anyone who
queries. They are unroutable from outside and Tailscale's security does not
depend on them being secret, so the practical risk is low. It is still
information you are choosing to publish, and anyone who dislikes that can use
Tailscale split DNS instead at the cost of configuring every client.

Write down which you chose and why.

## Acceptance

- From a tailnet device: `https://pc.mesh.shaul.dev/blog` loads with a valid
  certificate and no warning.
- From outside the tailnet: the name resolves and the connection times out. It
  must not reach anything, and it must not reveal anything beyond the address.
- Renewal exercised against the Let's Encrypt staging endpoint, including the
  distribution step.
- A host whose tailnet address changes gets its record reconciled without manual
  intervention.

## Out of scope

The public `shaul.dev` side (T13), and the tidy `mesh.shaul.dev/<name>` alias
that would need the Pi to route by path. Per-host names first.
