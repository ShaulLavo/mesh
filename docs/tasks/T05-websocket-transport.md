# T05 — WebSocket transport

**Status:** complete · **Blocked by:** nothing · **Owns:** `internal/transport/`

## Goal

Carry `internal/protocol` frames between hosts over `coder/websocket`, with the
reconnection behaviour the contract promises. Buildable and testable standalone,
before the daemon exists.

## Design

```go
package transport

// Conn carries protocol frames. A Unix socket and a WebSocket both satisfy it,
// which is what keeps the daemon a relay.
type Conn interface {
    ReadFrame() (protocol.Frame, error)
    WriteFrame(protocol.Frame) error
    Close() error
}

func Dial(ctx context.Context, url string, opts DialOptions) (Conn, error)
func Serve(w http.ResponseWriter, r *http.Request, h Handler) error
```

`ServeWithOptions` exposes keepalive, batching, and origin policy for the daemon.
`NewBatchingConn` exposes the same batching policy for another `Conn` adapter.
`NewStreamConn` adapts the daemon's Unix socket to the same interface.

Requirements from the plan:

- one WebSocket per client-to-host connection, sessions multiplexed by the session
  field already in the frame;
- **compression disabled** — PTY output is mostly small writes and compression
  adds latency and CPU for little gain;
- binary frames for data, text/binary JSON for control (pick one and document it);
- ping/pong keepalive with a read deadline that actually detects a dead link,
  which the TCP stack will not do for you on a sleeping laptop;
- output batched into reasonable chunks, coalescing small PTY writes;
- reconnect with exponential backoff plus jitter, capped; a reconnect resends
  `session.attach` with the client's last `seq`.

## Implementation

The transport pins `github.com/coder/websocket` at `v1.8.15`. Each WebSocket
message contains one complete Mesh frame and uses WebSocket binary mode,
including control frames. Both dial and accept set `CompressionDisabled`.

Every socket has a dedicated read pump, so pong processing does not depend on
the application calling `ReadFrame` at the right moment. The inbound queue holds
at most 256 frames and 8 MiB. A ping runs every 15 seconds and must receive its
pong within 5 seconds.

`Serve` batches contiguous output for the same session up to 32 KiB or 2 ms.
Control and input frames flush pending output first. `BatchingConn` copies every
buffered payload because PTY readers reuse their buffers. Handler completion
flushes queued output before closing the WebSocket. Direct `BatchingConn`
callers use `Flush` when they need graceful delivery; `Close` cancels queued
output so it can interrupt a blocked destination write.

`Dial` reconnects until `Close` cancels it. The delay starts at 100 ms, doubles,
adds 20 percent jitter, and never exceeds 5 seconds. It tracks the next byte
returned by `ReadFrame` for each attached session. A new WebSocket sends one
`session.attach` per session with that exact offset. Overlapping replay is
trimmed, while a gap closes the connection and returns an error. A snapshot
announcement leaves the prior offset committed until its complete
`KindSnapshot` frame is returned. If the link drops between those frames, the
next attach uses the prior offset.

Each installed socket has a generation. Inbound frames mutate resume state only
while their generation is current. The reconnect transition also spans an
outbound control write and its state update, so a concurrent reconnect cannot
omit a successful attach or replay a successful detach.

The transport never retries a failed `WriteFrame`. Retrying terminal input after
an uncertain write could duplicate keystrokes. The next operation reconnects,
and the caller decides whether its failed write is safe to repeat.

The encoder accepts additive session-scoped frame kinds that use
`session | payload`, including T01's snapshot frame. The protocol reader remains
the authority for which inbound kinds are valid.

## Acceptance

- Round-trip tests over `httptest` covering control, data, input, and snapshot
  frames.
- A test that severs the connection mid-stream and asserts the client reconnects
  and resumes at the correct `seq` with no gap and no duplication. This is the
  single most important test in the package.
- A test that a peer which stops responding to pings is detected and closed within
  the keepalive budget.
- Fuzz target for frame decoding (`protocol.Reader`) — cheap, and step 7 asks for
  protocol fuzzing anyway.

Implemented in `internal/transport` with tests for control, data, input, and
snapshot frames, two-session reconnect, exact resume without gaps or
duplicates, dead-peer detection, compression-off handshakes, output coalescing,
snapshot checkpoints, generation retirement, reconnect/control-write races,
and additive frame encoding. Run the focused checks with:

```bash
go test -race -count=100 ./internal/transport
go test ./internal/transport -run '^$' -fuzz '^FuzzProtocolReader$' -fuzztime=5s
```

## Note for later

The daemon's HTTP listener will host more than this WebSocket endpoint: step 8
mounts served sites and file browsers on it (T11). Reserve a path prefix for the
Mesh protocol now rather than assuming the listener is yours alone.

## Out of scope

Tailscale address discovery (T06) and authentication beyond "we are on the
tailnet." D23 records the resulting security boundary: this listener is
plaintext, and its confidentiality, integrity, and admission come from
Tailscale.
