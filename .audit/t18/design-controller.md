# T18 candidate: controller-owned claims and activation

## Problem

The durable T13 route table and the live proxy registry already share `Controller.commitGate`. T18 must join that ownership boundary: a separate tunnel manager cannot independently guarantee hostname collisions or prevent old connection cleanup from removing a reconnect. SQLite remains the durable collision authority, while `internal/tunnel` owns the signed protocol and SSH transport. T14 services remain separate.

## Usage (caller's view)

```sh
mesh serve claim vps blog.shaulavo.dev --yes
ssh -N -o ExitOnForwardFailure=yes -o IdentitiesOnly=yes \
  -i "$mesh_identity" -p 2222 \
  -R blog.shaulavo.dev:80:localhost:3000 vps.mesh.shaulavo.dev
mesh unserve blog.shaulavo.dev --host vps
mesh unserve blog.shaulavo.dev --local-edge
```

Daemon construction passes the existing controller to the SSH adapter and exposes signed claim frames through the existing control dispatcher. Authorization is one callback backed by sshd's existing safe authorized_keys reader; the controller calls it on each create and activation.

```go
controller, err := edge.NewController(ctx, edge.ControllerConfig{
    // Existing T13 dependencies, plus:
    TunnelState: store,
    AuthorizeTunnelKey: sshd.Authorizer(authorizedKeysPath),
})
err = sshd.Serve(ctx, sshConfig, tunnel.SSHOption(controller))
```

The SSH adapter needs one domain operation. Successful return means the route is installed; cleanup has no caller-supplied hostname or token that could target someone else's activation.

```go
release, err := claims.ActivateTunnel(ctx, principal, fullName, connectionEndpoint)
if err != nil { return false, nil }
connection.remember(fullName, release)
return true, nil
// cancel-tcpip-forward and disconnect synchronously call their saved release.
```

The CLI performs confirmation before preparing an outbox attempt. One operation drains an earlier attempt, persists the requested mutation, delivers it, and validates the exact receipt.

```go
err := store.DeliverTunnelMutation(ctx, tunnel.Intent{
    TargetID: edgeHost.ID, ClaimantID: identity.ID,
    Action: tunnel.Create, Hostname: fullName,
}, identity.PrivateKey, sendClaimToPinnedEdge)
```

## Shape

Domain types and exact signatures:

```go
// internal/tunnel/claim.go
// Parse validates domain, action, exact one-label hostname, identities,
// sequence 1..MaxInt64, Ed25519 signature and total frame <=4096 bytes.
type Action string
const ( Create Action = "create"; Release Action = "release" )
type Intent struct { TargetID, ClaimantID string; Action Action; Hostname string }
type Mutation struct { Intent; Sequence uint64; Signature []byte }
type VerifiedMutation struct { mutation Mutation; canonical []byte; digest [32]byte }
type Receipt struct { Sequence uint64; Digest [32]byte; Rejection string }
func Sign(Intent, uint64, ed25519.PrivateKey) (Mutation, error)
func Verify(Mutation, string) (VerifiedMutation, error)
func (VerifiedMutation) Mutation() Mutation
func (VerifiedMutation) Canonical() []byte
func (VerifiedMutation) Digest() [32]byte

// internal/tunnel/ssh.go
// Endpoint hides SSH channel numbers, bind tuples and channel wire payloads.
type Endpoint interface { Dial(context.Context) (net.Conn, error) }
type Activator interface {
    ActivateTunnel(context.Context, string, string, Endpoint) (release func(), err error)
}
func SSHOption(Activator) charmssh.Option

// internal/edge/tunnel_claim.go
// State reads and mutations run under existing commitGate.
type TunnelClaim struct { Hostname, ClaimantID string }
type TunnelState interface {
    ApplyTunnelMutation(context.Context, tunnel.VerifiedMutation) (tunnel.Receipt, error)
    LoadTunnelClaims(context.Context) ([]TunnelClaim, error)
    RecoverTunnelClaim(context.Context, string) error
}
func (*Controller) ActivateTunnel(context.Context, string, string, tunnel.Endpoint) (func(), error)
func (*Controller) RecoverTunnelClaim(context.Context, string) error
// Existing HandleControl decodes, verifies and applies claim mutations.

// internal/storage/tunnel.go
func (*Store) DeliverTunnelMutation(context.Context, tunnel.Intent, ed25519.PrivateKey,
    func(context.Context, tunnel.Mutation) (tunnel.Receipt, error)) error
func (*Store) ApplyTunnelMutation(context.Context, tunnel.VerifiedMutation) (tunnel.Receipt, error)
func (*Store) LoadTunnelClaims(context.Context) ([]edge.TunnelClaim, error)
func (*Store) RecoverTunnelClaim(context.Context, string) error
```

The storage mutation returns a receipt only for a conclusive decision. A rejection receipt still repeats sequence and digest, allowing the CLI to clear the exact pending attempt and report the refusal; a transport or database ambiguity retains it. This distinction is essential: a refused create cannot permanently block a later owner release in the same stream. A rejected new claimant allocates no high-water row. A consumed local sequence is never reused, including after conclusive rejection.

The transcript length-prefixes domain, action, exact edge ID and claimant ID with unsigned big-endian 64-bit byte lengths, inserts the unsigned big-endian sequence next, then length-prefixes the hostname. Sign SHA-256 over those bytes. The canonical transcript is not JSON; the bounded transport envelope may be structured control JSON. An outbox stores both exact canonical bytes and signature so retries never re-sign or derive fields from changed aliases.

Schema:

- `tunnel_claims(hostname PRIMARY KEY, claimant_id)`, indexed by claimant ID.
- `tunnel_highwater(claimant_id PRIMARY KEY, sequence, digest)`, permanently retained through release and recovery.
- `tunnel_outbox(edge_id, claimant_id, next_sequence, pending_action, pending_hostname, canonical, digest, signature, PRIMARY KEY(edge_id, claimant_id))`; pending columns are all-null or all-present. Store last consumed sequence rather than an increment that would overflow MaxInt64.

Use a per-stream OS file lock in the existing 0700 local state directory to serialize cross-process delivery, with short SQLite immediate transactions for allocate/sign/persist and exact-receipt clearing. Do not hold a database write transaction across the network. Process death releases the OS lock; its successor reads the pending attempt and delivers it before allocating anything. Pending-frame SQL checks prevent accidental overwrite even if a caller omits the process lock. Hash edge plus claimant into the lock filename; aliases do not participate. Windows needs the equivalent advisory file locking implementation if these CLI paths build there.

`Controller` owns `claims map[hostname]TunnelClaim` and `active map[hostname]*activation`, both only accessible under commitGate. An activation owns a unique token, endpoint, proxy transport and closed signal. A saved release closure takes commitGate and removes only its exact activation pointer, republishes, and closes its transport. It is idempotent, independent of canceled caller contexts, and finishes before cancellation acknowledgement. Startup restores claims with an empty active map. A failed channel dial invokes the same release before exposing the failure. Already accepted requests may fail closed, but subsequent requests get 404.

Extend `Registry` with one atomic complete-publication operation accepting existing origin routes and tunnel routes. It must build one immutable snapshot so T13 replacement cannot erase active tunnels. Represent tunnel destinations as a separate optional backend in the private proxy route; never fabricate a Tailscale endpoint, fake T11 service, or infinite heartbeat. Host validation, terminal exclusion, stable-client quotas and global work slots stay before either backend dispatch. Tunnel requests use a bounded HTTP transport and redact upstream errors. Inactive claims are omitted entirely from the HTTP snapshot.

`ApplyEdgeSnapshot` and `ApplyTunnelMutation` each recheck hostname collisions and combined route count inside their write transaction. A claim collides with any T13 row at that hostname, regardless of service path. T13 still permits its established path-sharing rules when no claim exists. SQLite transaction checks remain necessary even with the controller gate because direct storage callers and restart processes exist. Every same-owner create may update the stream high-water but leaves the claim unchanged. Release checks ownership and active state under the controller gate before committing; local recovery uses the same gate and preserves the high-water row.

State limits are checked before inserting: 32 claims/key, 8192 combined claims plus T13 route rows, 4096 high-water rows. Rate admission is bounded to 4096 entries with 52 ordinary and eight reserved tokens per minute; releases and exact sequence/digest retries can consume the reserved lane. Do not charge rejected unauthenticated input. Do not retain unbounded rate records for unaffiliated identities. Ensure rate state admission never blocks an already-durable owner's release: reserve/retain owner entries with their high-water identities or use a bounded fallback release bucket. The 60-frame limit still applies to repeated releases; capacity availability and rate permission are separate.

SSH wire parsing adapts Charm's tcpip-forward, cancel and forwarded-tcpip flow, without the listener implementation. It accepts exactly port 80 and the complete public hostname, authenticates Ed25519 Mesh principal from the connection, and invokes ActivateTunnel before success acknowledgement. Run one keepalive worker per authenticated connection: every 15 seconds send a reply-required request, expire after three unanswered intervals, synchronously release all activation tokens, then close. Bound outstanding keepalive requests rather than spawning a new blocked worker each interval. `-N` needs no session handler. Reject local forwarding channels.

Use explicit authenticated limits, initially eight simultaneous forwards/key and four channels/forward, with fixed per-channel buffer/window accounting. A stream-copy buffer cap alone does not bound SSH's receive windows: the implementation must include the x/crypto window and packet buffers in the per-forward byte ceiling, and its test must hold readers blocked to exercise that ceiling. Dial cancellation must close the owning SSH connection if the channel-open API cannot otherwise cancel, so blocked opens cannot accumulate goroutines.

Module ownership:

| Module | Knowledge owned |
| --- | --- |
| `internal/tunnel/claim.go` | Canonical transcript, signatures, bounded domain parsing |
| `internal/tunnel/ssh.go` | SSH global requests, channel conversion, connection lifetime, keepalive and byte accounting |
| `internal/edge/tunnel_claim.go` | Authorization decisions, existing mutation gate, claim and active ownership, rate lanes |
| `internal/edge/proxy.go` | One route snapshot and existing public ingress bounds |
| `internal/storage/tunnel.go`, migrations | Transactions, durable collisions/caps, replay tombstones, exact local outbox |
| `internal/cli/serve_claim.go` | Confirmation, local identity, edge pinning, request delivery and clear user errors |
| `internal/daemon` | Signed control dispatch and Unix-only recovery authorization |
| `internal/sshd` | One safe authorized_keys reader reused by handshake and claim/activation authorization |

Per boundary-discipline, wire validation happens before controller mutation. Per idempotence, cleanup captures activation identity and outbox retries preserve bytes. Per separate-before-serializing-shared-state, each local stream has its own process lock; commitGate is retained because public hostname ownership is genuinely shared. Interface depth is high: SSH callers ask for one activation and one cleanup closure; CLI callers request one delivered mutation, without sequencing storage primitives themselves.

## Synthesis decision

Candidate only; root synthesis selects the base.

## Tradeoffs accepted

- We accept a small tunnel branch in the existing registry in exchange for preserving one public ingress security boundary.
- We accept storage owning an outbox delivery operation in exchange for hiding crash and cross-process ordering from CLI callers.
- We accept a per-stream lock file beside SQLite in exchange for releasing ownership automatically on process death without holding SQLite writes across network I/O.
- We accept permanent replay rows and eventual refusal of new claimants at 4096 in exchange for preserving replay protection after releases.

## Alternatives considered

- Independent tunnel manager plus independent HTTP router: it hides SSH well but leaks hostname coordination and quota ordering into the daemon; it introduces two live route owners and cannot make T13 publication atomic without a new cross-manager protocol.
- Generalize every T13 origin into an arbitrary pluggable backend: it hides destination kinds but forces existing snapshots, pinning and heartbeat callers to learn a generic lifecycle that T13 does not need. A private route backend variant is sufficient.

## Open questions and risks

- Does the installed x/crypto SSH implementation expose enough flow control to enforce a defensible explicit per-forward byte ceiling, or should the initial channel limit be one with its fixed window as the documented ceiling?
- Should a conclusively refused frame carry a signed receipt, or is the existing identity-pinned control channel the receipt authentication boundary? The latter matches current edge registration.

## Next implementation step

Implement transcript golden tests and SQLite replay/collision/outbox transactions first, including conclusive rejection receipts and process-death recovery.
