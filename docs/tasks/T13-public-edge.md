# T13 — Public edge on the VPS

**Status:** not started · **Blocked by:** T11, T12 · **Owns:** `internal/edge/`

## Goal

`mesh daemon --edge` on the VPS: the one machine that faces the internet. It
holds TLS for the public names under `shaulavo.dev` and routes to origins over
Tailscale.

`mesh.shaulavo.dev` is the *private* name and never touches this edge. See
`docs/plan/03-serving.md` for the split, and T12 for the private side.

## Responsibilities

1. **TLS, conditionally.** The default arrangement is that something else owns
   port 443 on the VPS, terminates TLS, and reverse-proxies Mesh routes to the
   edge on a local port. In that arrangement the edge speaks plain HTTP and
   holds no certificates at all.
   Support owning 443 directly as the other arrangement, for a deployment where
   the site lives elsewhere. Reuse T12's DNS-01 plumbing there rather than
   writing a second ACME path.
   **Mesh never serves the `shaulavo.dev` site.** That is explicitly not its job.
2. **Routing.** Longest-prefix match on path, plus subdomain lookup for services
   that asked for one. Unknown route returns 404 without revealing what else
   exists.
3. **Registration.** Origins push their public services and refresh on a
   heartbeat. Accept only host identities already known to this edge (T06). A
   registration naming a route another host already holds is refused, not
   silently taken over.
4. **Proxying.** Stream to the origin over Tailscale. No buffering of bodies.
   WebSocket upgrades pass through. Sensible timeouts, and a request budget so
   one slow origin cannot exhaust the edge.
5. **Offline origins.** Return 502 with a page that says which machine is down
   and when it was last seen. If the service opted into `--wake-on-request`, ask
   the Pi to wake it (step 6) and hold the request with a bounded deadline.

## The part that needs care

This is the only Mesh component with a public attack surface. Everything else
hides behind Tailscale. Treat it accordingly:

- do not echo internal hostnames, tailnet addresses, or paths in error pages;
- bound header size, body size, and concurrent connections per origin;
- rate limit by IP before any proxying happens;
- log enough to investigate abuse without logging request bodies.

## Acceptance

- End to end against a real domain: a request to the public name reaches a static
  service on another machine over Tailscale, with a valid certificate.
- Certificate renewal tested against the Let's Encrypt staging endpoint.
- Origin killed mid-request: edge returns 502 promptly, does not hang, does not
  leak the origin's address.
- Route collision between two hosts is refused with a clear error to the second.
- The edge runs correctly behind a TLS-terminating proxy on a non-443 port, with
  `X-Forwarded-Proto` respected so generated links stay `https`.
- The terminal session path still never transits the edge. Assert it.

## Out of scope

Per-service authentication, multi-user access, the private `mesh.shaulavo.dev` side
(T12), and claiming any name the user did not explicitly ask for.
