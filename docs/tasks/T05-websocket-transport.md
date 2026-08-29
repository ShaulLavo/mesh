# T05 — WebSocket transport

**Status:** not started · **Blocked by:** nothing · **Owns:** `internal/transport/`

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

## Acceptance

- Round-trip tests over `httptest` covering control + data + input frames.
- A test that severs the connection mid-stream and asserts the client reconnects
  and resumes at the correct `seq` with no gap and no duplication. This is the
  single most important test in the package.
- A test that a peer which stops responding to pings is detected and closed within
  the keepalive budget.
- Fuzz target for frame decoding (`protocol.Reader`) — cheap, and step 7 asks for
  protocol fuzzing anyway.

## Out of scope

Tailscale address discovery (T06), authentication beyond "we are on the tailnet"
(the daemon binds to the Tailscale interface; document the assumption).
