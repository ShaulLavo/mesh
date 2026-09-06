# Registry-owned public names

## Problem

T18 adds durable hostname reservations and ephemeral SSH backends to an edge whose Controller owns durable T13 mutations while Registry owns the published HTTP table. `Controller.register` currently holds `commitGate` across SQLite apply and `Registry.Replace`, and reconstructs all routes after every registration. A tunnel-only overlay therefore cannot simply call Replace: the next T13 registration would erase it. SQLite currently checks `(public_name, service_name)` collisions, which is insufficient for whole-hostname claims. Existing T16 changes remain outside this design.

## Usage (caller's view)

The CLI consumer performs one durable operation:

```go
client := tunnel.NewClient(store, identity, sendClaim)
ack, err := client.Mutate(ctx, edgeID, tunnel.Create, "blog.shaulavo.dev")
```

`Mutate` obtains the cross-process stream lock, finishes any stored attempt exactly, allocates and persists the requested mutation, sends it, checks its acknowledgement, and returns. The CLI confirms public exposure before making this call. A successful acknowledgement lets it print the exact hostname and stock SSH command.

The SSH adapter invokes an edge capability without knowing reservation persistence:

```go
lease, err := registry.ActivateTunnel(ctx, claimantID, fullName, backend)
if err != nil { return false, nil }
connection.addForward(fullName, lease)
return true, nil
```

The adapter attaches successful leases to the SSH connection before returning success. Cancellation closes the matching lease before returning its acknowledgement. Connection shutdown closes all leases before closing the connection. Registry wraps backend opens so a failed channel open closes the matching lease before returning to HTTP.

Existing T13 registration keeps origin authentication and pinning in Controller:

```go
// After pinning, the controller enters its registry's private mutation gate.
// ApplyEdgeSnapshot transactionally checks durable tunnel reservations too.
err := c.state.ApplyEdgeSnapshot(ctx, snapshot, digest, receivedAt)
// Reconciliation replaces only T13 state and merges active tunnels.
err = c.registry.replaceOriginsLocked(routes)
```

## Shape

Registry becomes the single owner of publication mutation ordering. Controller's existing `commitGate` moves to Registry. Controller's existing acquire/release helpers use that private gate; tunnel methods use the same gate. No external caller receives a mutex or callback-based transaction API. Controller still owns origin proof, pinning, liveness and reconciliation. This is a structurally distinct boundary from placing all tunnel ownership in Controller: public hostname publication owns tunnel reservations and activations directly.

```go
// internal/tunnel/mutation.go; transport-independent domain values
type Action string
const (Create Action = "create"; Release Action = "release")
type Mutation struct {
    EdgeID, ClaimantID string
    Sequence uint64
    Action Action
    Hostname string
    Signature []byte
}
type Attempt struct {
    Mutation Mutation
    Canonical []byte
    Digest [32]byte
}
type Ack struct { Sequence uint64; Digest [32]byte }
func Sign(edgeID string, sequence uint64, action Action, hostname string,
    private ed25519.PrivateKey) (Attempt, error)
func Verify(m Mutation, targetID string) (Attempt, error)

// internal/tunnel/client.go
type Deliver func(context.Context, Attempt) (Ack, error)
func NewClient(store OutboxStore, key ed25519.PrivateKey, send Deliver) *Client
func (*Client) Mutate(context.Context, string, Action, string) (Ack, error)

// internal/edge/tunnel_claim.go
type TunnelConfig struct {
    TargetID string
    State TunnelStateStore
    Authorized func(string) bool
}
func (r *Registry) RestoreTunnels(context.Context, TunnelConfig) error
func (r *Registry) ApplyTunnelClaim(context.Context, tunnel.Mutation) (tunnel.Ack, error)
func (r *Registry) ReleaseTunnelLocal(context.Context, string) error
func (r *Registry) ActivateTunnel(context.Context, string, string, tunnel.Backend) (io.Closer, error)

// internal/tunnel/ssh.go; edge imports tunnel, tunnel does not import edge
type Backend interface {
    Open(context.Context) (net.Conn, error)
    Close() error
}
type Activator interface {
    ActivateTunnel(context.Context, string, string, Backend) (io.Closer, error)
}
func SSHOption(Activator) charmssh.Option
```

`RestoreTunnels` runs once during daemon construction, before public listeners start. A constructor option would eliminate the initialization phase, but extending the existing Registry constructor with optional dependencies is a larger API change. If this candidate wins, prefer a single construction path over permitting arbitrary restoration calls at runtime.

Registry contains separate maps for durable reservation records and token-bound active backends, plus its existing origin publication snapshot. Origin reconciliation replaces only the origin table. A common publication builder merges live tunnels at the atomic HTTP snapshot boundary, per separate-before-serializing-shared-state. The shared gate is still required because release/activation and durable T13 publication protect one hostname ownership invariant. Claims consume one combined-route slot; activation does not consume a second slot.

Each private lease stores hostname, unique activation token, backend and once-close state. Closing conditionally removes only a matching current token. Removal occurs under the mutation gate; transport close happens after releasing the gate. Backend Open errors use the same token removal. HTTP never checks authorization or SQLite. It sees an atomic active route whose backend is already installed. Inactive claims have no HTTP route and yield 404. Route structs gain a private tagged backend variant rather than a fake Tailscale endpoint or fabricated T11 service, per model-the-domain.

The signed canonical transcript is exactly `len(domain)||domain||len(action)||action||len(edge)||edge||len(claimant)||claimant||uint64(sequence)||len(host)||host`, with every length a big-endian uint64. Sequence is 1..MaxInt64. Parsing enforces Ed25519 identities, a complete one-label hostname and a 4 KiB canonical-plus-signature bound. The digest is SHA-256; the signature signs that digest. JSON is only an envelope.

SQLite gains claim, claimant high-water and local stream-outbox tables. `ApplyTunnelClaim` verifies the signature and current authorization for creates, classifies exact retry, rate-limits, checks active release refusal, and calls one storage transaction. That transaction writes high-water and claim mutation atomically, rechecking whole-hostname T13 collisions and all durable caps. `ApplyEdgeSnapshot` reciprocally checks claims and combined counts in its transaction. Local release deletes the claim only and leaves high-water untouched. Claims carry enough owner identity to verify releases after authorization removal.

Outbox sequence allocation must acquire a SQLite write lock before reading sequence (`BEGIN IMMEDIATE`, or first-write locking); existing deferred `BeginTx` is not proof of this requirement. The exact attempt is committed before delivery. A per-(edge,claimant) advisory lock serializes delivery across CLI processes and is automatically released on process death; hold no SQLite transaction over network I/O. The next process loads and resends the existing canonical bytes, digest and signature before allocating a successor. Matching acknowledgement uses compare-and-delete/update, preserves the last allocated sequence, and never clears another attempt. CLI startup of this operation performs recovery even when its requested mutation differs from the pending one. Do not allocate a new signed sequence after ambiguous send failure.

Creation and activation consult the same safely opened authorized_keys implementation as sshd; export a key-ID authorizer there rather than reimplement its ownership/mode checks. Release verifies its actual owner signature without requiring current authorization. Local recovery is routed by daemon's existing Unix-only `localClientKey`, not a request boolean accepted from the network.

SSH's protocol adapter validates exact bind hostname and port 80, reuses Charm request/channel encodings, and never calls net.Listen. It starts connection lifetime tracking on the first relevant global request, not a session handler, so `ssh -N` works. Keepalive sends one reply-required request per 15 seconds, closes after three missed replies, and must use a timer independent of the potentially blocked SendRequest goroutine. The three-miss path removes leases before closing the connection.

Authenticated limits include forwards aggregated per claimant across connections, open channels per forward, and an acquired byte allowance before opening each channel. Installed x/crypto's channel.go sets receive windows to 2 MiB and max packets to 32 KiB: fixed io.Copy buffers alone cannot establish a byte bound. Account for that window, HTTP response-header ceiling and proxy buffers in each channel reservation; cap simultaneous channels and chunk writes. All channels must be tracked and closed when the activation ends. Preserve edge stable-client rate/concurrency and global upstream gates before opening tunnel channels.

For mutation rate limits, use 60 total tokens and at most 52 create-only tokens per key per minute, preserving eight for releases and exact retries. At the 4,096-entry cap, durable owners must retain a rate entry or displace entries belonging to keys with no durable state, so unaffiliated frame floods cannot prevent release admission. Bounds can refuse excess release frames; durable-state saturation cannot itself forbid owner releases.

Interface depth is strong at the edge boundary: ActivateTunnel hides authorization, collision, limits, token generation and publication behind one call and a close handle. It avoids exposing SSH wire types. The tradeoff is that Registry now depends on durable tunnel state and authorization, whereas it was purely HTTP publication. The existing Controller gains little new public surface but requires all Replace paths to be split correctly into locked internals and safe public methods.

## Synthesis decision

Independent registry-centric candidate; parent synthesis chooses the base. The alternative Controller-owned gate is likely cheaper if preserving existing ownership outweighs consolidating publication ownership.

## Tradeoffs accepted

- We accept moving one existing mutation gate in exchange for Registry owning all hostname publication ordering.
- We accept a process advisory stream lock in addition to SQLite atomic allocation in exchange for strict cross-process delivery serialization without network transactions or expiring lease races.
- We accept conservative channel concurrency in exchange for an auditable byte bound using the SSH library's existing receive windows.
- We accept separate persistent claims and runtime activations in exchange for startup always restoring inactive names and stale cleanup being harmless.

## Alternatives considered

- Controller owns everything: preserves existing gate and store boundary with fewer integration edits; its public interface becomes a larger mix of origin enrollment, signed claim control and SSH lifecycle, though that may be the best practical depth tradeoff.
- Separate Manager sharing an exported gate: exposes lock order and mutation phases to two public owners, a shallow module and a future bypass risk.
- Independent tunnel HTTP handler: makes unified global request budgets and host/path collision ownership external coordination responsibilities, and risks terminal-path bypass.

## Open questions and risks

- Can every current Registry.Replace caller be migrated without a gate reentrancy deadlock, including Controller reconciliation failure paths?
- Does the repository intend cross-process stream serialization to be SQLite-only? If so, how should a durable lease distinguish a killed sender from a merely slow sender without reopening duplicate-delivery races?
- Will the byte limit be documented as total bounded buffering per forward, including SSH's fixed receive window, rather than only application copy buffers?

## Next implementation step

Implement golden transcript fixtures and crash-recoverable cross-process outbox tests first, then verify whole-hostname collision transactions against T13 publication before wiring the SSH backend.
