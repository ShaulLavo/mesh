# T13 — Public edge on the VPS

**Status:** done · **Blocked by:** nothing · **Owns:** `internal/edge/`

## Goal

The VPS daemon is the one Mesh process that faces the internet. It routes public
service requests to origins over Tailscale. By default, a separate front door
terminates TLS and forwards requests to a loopback-only Mesh listener. An
optional direct-TLS mode lets Mesh own a public listener and the
`*.shaulavo.dev` certificate.

`mesh.shaulavo.dev` is the *private* name and never touches this edge. See
`docs/plan/03-serving.md` for the split, and T12 for the private side.

## Responsibilities

1. **TLS, conditionally.** The default arrangement is that something else owns
   port 443 on the VPS, terminates TLS, and reverse-proxies Mesh routes to the
   edge on a local port. In that arrangement the edge speaks plain HTTP and
   holds no certificates at all.
   Support owning 443 directly as the other arrangement, for a deployment where
   the site lives elsewhere. Reuse T12's DNS-01 plumbing, but use a distinct
   `public-edge` certificate profile and serving slot.
   **Mesh never serves the `shaulavo.dev` site.** That is explicitly not its job.
2. **Routing.** Match an exact public hostname and then the longest service path.
   A public hostname is exactly one label below `shaulavo.dev`. Reject the apex,
   nested names, and `mesh.shaulavo.dev`. Unknown and reserved routes return 404
   without revealing what else exists.
3. **Registration.** Origins push signed complete snapshots and refresh them on
   a heartbeat. Accept only identities in the edge's explicit allowlist. A route
   claimed by one origin remains owned while that origin is offline; another
   origin cannot take it over.
4. **Proxying.** Stream to the origin over Tailscale. No buffering of bodies.
   WebSocket upgrades pass through. Sensible timeouts, and a request budget so
   one slow origin cannot exhaust the edge.
5. **Offline origins.** Return 502 with a page that says which machine is down
   and when it was last seen. Keep the wake operation behind a bounded interface,
   and support a public `--wake-on-request` route once the target allows waking.

## Authenticated complete snapshots

Each snapshot binds the exact edge identity, origin identity, monotonic sequence,
issue and expiry times, and the origin's complete sorted route set. The origin
signs the canonical transcript with its Mesh Ed25519 identity. An empty higher
sequence withdraws every public route for that origin.

The origin consumes a sequence in its SQLite outbox before sending it. It accepts
only an acknowledgement with the same sequence and digest. An ambiguous attempt
is retried exactly; changed desired state advances to a new sequence. Service
writes and publication share one gate, so a heartbeat cannot publish stale state
over a newer mutation. A public upsert that the edge does not acknowledge rolls
back locally. A delete stays deleted and continues retrying publication for T14.
SQLite stores signed 64-bit integers, so the storage boundary accepts only edge
sequences in `1..math.MaxInt64`. It rejects larger inbound values and corrupt
nonpositive rows instead of wrapping them across the signed/unsigned boundary.

The edge verifies the signature and both identity pins before one SQLite
transaction replaces the origin's snapshot and claims. A lower sequence is
stale. An equal sequence with the same digest is an idempotent acknowledgement
and does not refresh liveness. An equal sequence with a different digest is a
conflict. Cross-origin `(public name, service path)` collisions fail inside the
same transaction. Restart restores verified claims as offline, which preserves
ownership without trusting a stale network endpoint.

Liveness starts when the edge accepts a new snapshot. The signed snapshot TTL is
applied to that receipt time, so clock skew at an origin cannot extend its online
window. The edge retains the claim after that window expires and returns 502
until a newer authenticated snapshot makes the origin live again.

## Endpoint and identity pinning

Snapshots cannot choose an upstream address. The edge resolves the exact
allowlisted Tailscale name, rejects non-Tailscale and wrong-port results, opens a
connection to that numeric endpoint, and verifies `host.info` against the
allowlisted Mesh identity before committing the snapshot. The origin performs
the same resolution and `host.info` check against its configured edge identity
before registration or `edge.list`.

`edge.list` is not a public status endpoint. Each bounded page request carries a
fresh origin signature over the target, origin, request ID, cursor, limit, and
issue time. The edge checks that proof against the same allowlist.

## Public request boundary

This is the only Mesh component with a public attack surface. Everything else
hides behind Tailscale. Treat it accordingly:

- The default proxy mode binds only a numeric loopback address. It trusts
  forwarded metadata only from that loopback peer and requires exactly one
  canonical `X-Forwarded-For` address and one `X-Forwarded-Proto` value.
- Direct-TLS mode binds an unspecified or public unicast address. It requires
  TLS and an exact valid SNI/Host pair.
- Both modes hard-return 404 for the terminal control path, including repeated
  percent-encoding. Terminal controls are handled only by the tailnet or local
  control listeners.
- Request bodies, response headers, total upstream work, and per-origin upstream
  work are bounded. The public server has a generous hard 10-minute deadline for
  reading a complete request, including its body. Dial and response-header waits
  also have deadlines. There is no write deadline: HTTP responses and WebSocket
  upgrades continue to stream.
- Stable-address quotas aggregate IPv4 addresses individually and IPv6 clients
  by `/64`. They shed simple repeated floods; they are not the defense against
  address rotation. The global and per-origin concurrency channels are the hard
  work bounds. A full rate table admits new addresses without tracking them
  rather than letting address cycling evict a live limited entry.
- Public errors expose only the configured display alias and last-seen time.
  Logs contain event metadata, never request bodies, credentials, origin
  addresses, or untrusted error text.

## Certificate separation

T12's signed certificate protocol now uses a profile-bound v3 transcript. Its
length-prefixed private-name field is canonical for `private-origin` and empty
for `public-edge`. The private wildcard remains in
`private-tls/{live,staging}` for upgrade continuity. Direct-TLS edge
certificates use the separate `public-edge` profile, `*.shaulavo.dev` name, and
`certificates/public-edge/{live,staging}` store. An installer rejects a bundle
for the other profile. The renewer also isolates its public ACME account and
certificate state under `public-edge/{live,staging}`.

Proxy mode constructs neither the public certificate installer nor its TLS
source. Its runtime configuration rejects a certificate-renewer identity. DNS
provider tokens and ACME account keys stay on the renewer and never appear in an
origin or edge runtime configuration. The daemon also rejects combining
`--edge` with the Pi-only `--private-names-config` role.

The public certificate manager owns only ACME DNS-01 TXT records. It does not
create or change public A, AAAA, or CNAME records. The operator points each
public service name at the selected front door.

## Runtime configuration

Both edge configuration files are bounded, strict JSON. Their paths must be
clean and absolute. The VPS allowlist file for the default proxy arrangement is:

```json
{
  "mode": "proxy",
  "listenAddress": "127.0.0.1:8080",
  "origins": [
    {
      "identity": "<ORIGIN_MESH_IDENTITY>",
      "displayAlias": "desktop",
      "tailscaleName": "desktop.example.ts.net",
      "controlPort": 7337,
      "websocketPath": "/mesh"
    }
  ]
}
```

Start the VPS daemon with its normal tailnet control listener and the allowlist:

```bash
mesh daemon --tailnet-port=7337 --websocket-path=/mesh \
  --edge=/home/shaul/.config/mesh/public-edge.json
```

The front door must connect from loopback, preserve the public `Host`, and set
exactly one `X-Forwarded-For` address and one `X-Forwarded-Proto` value. Proxy
mode rejects `certificateRenewerId` and never opens the public certificate
stores.

Each origin pins that VPS with a separate target file:

```json
{
  "identity": "<VPS_MESH_IDENTITY>",
  "tailscaleName": "vps.example.ts.net",
  "controlPort": 7337,
  "websocketPath": "/mesh"
}
```

Start that origin with:

```bash
mesh daemon --tailnet-port=7337 --websocket-path=/mesh \
  --public-edge-target=/home/shaul/.config/mesh/public-edge-target.json
```

For direct TLS, change the VPS file to `"mode": "direct-tls"`, bind `:443`
or one exact public unicast address, and add
`"certificateRenewerId": "<PI_MESH_IDENTITY>"`. The operating system must
already permit the daemon to bind that address and port. Mesh does not grant
capabilities, open a firewall, or configure public address records.

For the direct-TLS arrangement only, add this object to the Pi's existing T12
private-names configuration:

```json
"publicEdge": {
  "tailscaleName": "vps.example.ts.net",
  "identity": "<VPS_MESH_IDENTITY>",
  "controlPort": 7337,
  "websocketPath": "/mesh"
}
```

The Pi then runs public renewal independently from private DNS reconciliation.
The existing `mesh private-names reconcile` staging or live command runs both
configured certificate passes and distributes the public bundle only to the
pinned VPS.

Startup verifies every restored signed claim before binding the public listener.
Invalid configuration, corrupt edge state, a failed required bind, or a public
listener failure is fatal. Shutdown closes the public server with a bounded
grace period. Public-route heartbeat failures are reported and retried without
stopping the daemon. A confirmed local Tailscale address-set change returns a
nonzero restart error so the T10 supervisor rebinds all control listeners
without operator action.

## Implementation

The component and daemon runtime wiring is complete. Verification included a
clean formatting, generation, module-tidiness, and vet pass; the full race test
suite; the retained Host fuzzer; all 17 integration scripts; Darwin arm64
compilation for the command, daemon, DNS, edge, serving, and transport packages;
and a Darwin arm64 Mesh binary build.

- `internal/edge/snapshot.go`, `registration.go`, and `publisher.go` implement
  signed snapshots, identity-pinned registration, authenticated status pages,
  and the durable origin outbox.
- `internal/edge/proxy.go` owns the immutable route table, longest-path routing,
  offline behavior, the bounded wake interface, trust-mode checks, streaming
  reverse proxy, and public resource budgets. Inbound body deadlines return a
  fixed 408 rather than being misreported as an offline origin. Production
  wiring uses T19's target-authorized wake client and LAN sender selection.
- `FuzzCanonicalPublicHost` exercises the public Host parser's canonicalization
  and round-trip invariants. It exposed and closed acceptance of an empty host
  with a port and bracketed DNS names.
- `internal/edge/config.go` strictly parses bounded edge and target JSON files.
  Proxy mode defaults to `127.0.0.1:8080`; direct-TLS mode defaults to `:443`.
  Proxy mode requires loopback. Direct-TLS mode accepts only an unspecified
  address or a non-private, non-Tailscale public unicast address.
- Migration `00003_edge.sql` stores signed edge snapshots, durable route claims,
  last-seen state, and each origin's exact outbound registration attempt.
- The daemon service controller serializes database changes with complete
  snapshot publication and performs an immediate sync followed by one-minute
  heartbeats.

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

Automated acceptance must require no real domain, Tailscale account, Cloudflare
token, or ACME account. It additionally proves:

- Real daemon processes with local identity, resolver, and certificate fixtures
  exercise both proxy and direct-TLS modes.
- A request to a public name reaches a static service on another daemon through
  the edge. WebSocket traffic streams through the same route.
- Proxy mode starts without reading or constructing public certificate state.
  Direct-TLS mode serves a profile-bound `*.shaulavo.dev` certificate, and a
  staging bundle cannot change its live listener.
- Proxy mode rejects malformed or repeated forwarded headers and preserves the
  single trusted `X-Forwarded-Proto` value upstream.
- A daemon restart restores signed claims as offline. A later authenticated
  snapshot restores liveness without releasing ownership during the gap.
- T19 supplies the bounded production wake client. A wake-enabled route waits
  for the authorized target and a fresh publication before forwarding.
- Public service upsert and delete controls do not return success before the
  edge acknowledges the exact resulting snapshot. T14 relies on this seam.
- A running binary, not only the handler, returns 404 for the terminal path on
  the public listener.

The real-domain and Let's Encrypt staging checks are operator-only. They are
unavailable in the credential-free development and CI environments and remain
required before production use. Run staging issuance before requesting a live
`public-edge` certificate.

## Out of scope

Per-service authentication, multi-user access, the private `mesh.shaulavo.dev` side
(T12), and claiming any name the user did not explicitly ask for.
