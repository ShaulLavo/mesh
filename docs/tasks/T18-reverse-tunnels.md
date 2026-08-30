# T18 — Reverse tunnels through the edge

**Status:** not started · **Blocked by:** T13, T15 · **Owns:**
`internal/tunnel/`, `internal/cli/serve_claim.go`, `internal/edge/tunnel_claim.go`

## Goal

```bash
mesh_identity="${MESH_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/mesh}/identity.key"
mesh serve claim vps blog.shaulavo.dev
ssh -N -o ExitOnForwardFailure=yes -o IdentitiesOnly=yes -i "$mesh_identity" \
  -p 2222 \
  -R blog.shaulavo.dev:80:localhost:3000 vps.mesh.shaulavo.dev
```

Something running on the machine in front of you, reachable from outside the
tailnet, gone the moment you disconnect. The one capability Tailscale cannot
provide, because it needs a publicly routable machine and step 8 already put one
there.

**Read D22 before adding anything to this.** Sharing work with other people is a
separate product. This exists so you can reach your own machine, and every
feature that only makes sense because somebody else is looking at the result
belongs somewhere else: preview URLs per build, share links, expiring access,
anything that turns a route into an artifact with a lifecycle.

## The rule this must not break

The overview rules out arbitrary tunnels and that stays true. D20 defines what is
allowed, and every clause is a test:

- authenticated by an authorized key (T15);
- bound to the exact public hostname the user **named** in both the claim and
  forward request, never derived;
- refused when another host already holds that name;
- refused when the name was not claimed under D15's public-exposure rules;
- unpublished the instant the SSH connection drops.

D14 is unaffected. Tunnelled HTTP is not terminal traffic, and terminal sessions
still go direct to the origin.

## Responsibilities

1. **Accept a constrained `tcpip-forward`.** `charmbracelet/ssh`'s `tcpip.go`
   implements the channel flow and ships an `ssh-remoteforward` example. Reuse
   its protocol handling, but never call `net.Listen` for the requested tuple.
   The bind address is the claimed full hostname and the only accepted bind port
   is `80`, a protocol marker for the edge's existing HTTP front door.
2. **Reserve, then activate.** Add `mesh serve claim EDGE FULLNAME [--yes]` for
   durable, inactive claims. A successful forward activates its matching claim
   as an edge route pointing at the SSH connection instead of a Tailscale
   origin. Claims and T13 routes share one collision check.
3. **Unregister on close.** Connection drop, network death, and daemon shutdown
   all unpublish the active route but retain its inactive claim. A stale route
   pointing at a dead channel is the failure mode that makes this feature
   untrustworthy.
4. **Bound it.** Cap concurrent forwards per key and bytes in flight per forward.
   T13's stable-address quota sheds simple floods, while its global concurrency
   channel is the hard public work bound. This task must also bound the
   authenticated side.

## The part that needs care

A T14 service route is not a tunnel reservation. Do not create a dummy service
or replace a live service to claim a tunnel name.

T18 adds `mesh serve claim EDGE FULLNAME [--yes]`. `FULLNAME` must be one
complete, one-label `*.shaulavo.dev` hostname. The command prints that hostname
and uses the same public confirmation as T14. It loads the local Mesh identity
from the state directory. The exact stock SSH command uses that same private key
with `-p 2222`, `-i`, and `IdentitiesOnly`; a default SSH key is not the same
principal.

Create and release use one canonical `mesh/tunnel-claim/v1` transcript. Hash an
eight-byte big-endian length and the bytes of the domain, action (`create` or
`release`), exact target edge identity, claimant key ID, and full hostname, in
that order; encode the monotonically increasing sequence, constrained to
`1..MaxInt64`, as one big-endian `uint64` between the claimant ID and hostname.
Sign the SHA-256 digest. No JSON, map ordering, platform path, or display alias
enters the signature.

Sequence allocation and delivery use a transactional cross-process outbox in
the local SQLite store. One transaction locks the `(edge, claimant)` stream,
allocates its next sequence, and stores the exact canonical bytes, digest,
signature, action, and hostname before any send. Another CLI process finishes
that pending mutation before allocating a successor. Startup retries the stored
attempt byte for byte. The outbox clears only after an acknowledgement repeats
the exact sequence and digest.

The edge atomically persists each claimant's high-water sequence and digest
with the claim mutation. That one high-water row survives releases as the replay
tombstone; it is not one row per mutation. An equal sequence and digest is an
idempotent acknowledgement; a lower sequence or equal sequence with another
digest is rejected. An ambiguous result retries the stored signed mutation
rather than issuing a new one.

Creation and activation require the claiming key to be present in the edge's
current T08-managed `authorized_keys`. A missing, unreadable, or insecure file
fails closed. Release requires the exact claim owner's signature; another
authorized key cannot release it. Owner-signed release remains allowed after
that key is removed from `authorized_keys`, so revocation does not make a claim
undeletable. Authorization-file changes never silently release durable claims.
If the owner key is lost, an operator can release through T18's edge-local
`mesh unserve FULLNAME --local-edge` path, which uses only the existing `0700`
daemon socket and is unavailable over a tailnet listener. Local recovery keeps
the retired owner's sequence high-water mark and tombstone, so it cannot make an
old signed create valid again.

A claim is a durable, inactive reservation. It creates neither a T11 service nor
a T13 origin route, and the edge returns 404 while only the reservation exists.
The `tcpip-forward` request must authenticate with the claiming key and repeat
the exact full hostname as its bind address. Never derive `.shaulavo.dev` from a
short name.

The reservation owns the whole hostname. Claim creation and T13 publication use
one serialized collision transaction, so any T13 route on that hostname
conflicts even when its path differs. A claim by the same owner for the same
hostname is a convergent no-op. Another owner on that hostname conflicts; other
hostnames are independent. Acknowledge the SSH forward only after the edge
installs the tunnel route.

Durable and ingress state is bounded. One key may hold at most 32 hostnames.
Claims and T13 routes share the edge's existing 8,192-route global ceiling. The
edge retains at most 4,096 claimant high-water rows and 4,096 bounded mutation
rate entries. A canonical mutation, including signature, must fit in 4 KiB. One
key may send at most 60 authenticated claim frames per minute; eight tokens are
reserved for releases and exact idempotent retries. Retries allocate no new
durable state. Reaching a state cap refuses new creates but never prevents an
owner release or edge-local recovery.

Only the claim, sequence, digest, and release tombstone are durable. Every active
route is in memory and bound to a unique SSH connection and activation token.
Startup restores every claim inactive and returns 404. Connection close and
`cancel-tcpip-forward` conditionally remove only their own token before cleanup
is acknowledged, so an old connection cannot remove a newer reconnect. Claim
creation, release, activation, deactivation, and T13 publication share the same
mutation gate. `mesh unserve FULLNAME --host EDGE` owner-signs release of an
inactive claim and refuses while its tunnel is active.

The server sends a reply-required SSH keepalive every 15 seconds and closes the
connection after three missed replies. A failed channel open deactivates its
token immediately. Either path conditionally removes the active route before
closing, so a silent partition becomes inactive 404 within 60 seconds rather
than leaving a hanging or stale public hostname.

The server rejects a bind address that is short, wildcard, loopback, an IP, or
not the exact claimed hostname. It rejects port zero and every port except `80`.
It never opens the requested network listener: the established edge HTTP server
routes the claimed hostname into SSH channels. The claim stores authorization
only. T18 adds no payload storage, preview link, expiring share, or deployment
state.

## Acceptance

- End to end: `ssh -R` from a machine, request the public name, get the local
  server's response. The documented `-N` command stays connected without
  opening a default session.
- Disconnect, then request again: 404 from the edge, not a hang and not a 502
  that names the machine.
- A second host requesting a held name is refused with a clear error.
- A forward naming an unclaimed route is refused.
- Cancelling claim confirmation creates no reservation; `--yes` creates the
  same signed reservation without a prompt.
- A claim alone returns 404. A matching authenticated forward activates it;
  disconnect returns the edge to 404; reconnect with the same key works without
  reclaiming.
- A wrong key, a short hostname, an unclaimed hostname, and a second active
  forward are refused, and `ExitOnForwardFailure` makes the documented client
  exit nonzero instead of remaining connected without a working forward.
- Wildcard, loopback, IP, port-zero, and non-80 forward requests are refused,
  and the SSH handler never creates a network listener for the requested tuple.
- Exact create/release retries acknowledge the original sequence and digest.
  Lower, replayed post-release, cross-edge, wrong-owner, and equal-sequence
  conflicting mutations are refused. A same-owner claim rerun is a no-op.
- Race two CLI processes and kill one after send. They serialize the outbox,
  restart with the exact stored signature, and clear it only on the matching
  acknowledgement. Golden transcript bytes prevent encoding drift.
- Hit the 32-claim per-key, 8,192 combined-route, 4,096 high-water/rate-entry,
  4-KiB frame, and 60-frame-per-minute bounds, including the reserved release
  lane. New creates fail at each cap; owner release and local recovery still
  work.
- Race a T13 publication against a tunnel claim for the same hostname. Exactly
  one wins, including when the T13 service uses a non-root path.
- `mesh unserve FULLNAME --host EDGE` releases an inactive claim, refuses an
  active one, rejects a different signer, and does not disturb T14 services.
- Restart the edge with a durable claim and an active tunnel: it returns with
  the claim inactive and serves 404. Race old connection cleanup against a new
  activation and prove the old token cannot remove the new route.
- Blackhole an active SSH connection without a clean TCP close. Keepalive expiry
  deactivates exactly its token and the hostname returns 404 within 60 seconds.
- Removing a key from `authorized_keys` blocks new claims and activations but
  preserves its claims and permits their owner-signed release. Missing or
  insecure authorization state fails closed. The local recovery path works only
  through the edge's Unix socket.
- `cancel-tcpip-forward` removes its token-bound route before acknowledgement and
  leaves the durable claim inactive.
- Concurrent forward and byte limits are enforced, with tests that hit them.
- Terminal traffic still does not transit the edge. Same assertion T13 makes.

## Out of scope

Local forwarding (`ssh -L`), which Tailscale already does better. UDP. Anything
that forwards to a destination other than the connecting client's own machine.
