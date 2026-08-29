# T02 — Bounded outbound queue per attachment

**Status:** not started · **Blocked by:** nothing · **Owns:** `internal/worker/worker.go`

## Goal

The PTY reader must never block on a slow or dead client.

## The current behaviour

`Worker.pump` holds `w.mu` and writes directly to the attached client's socket
with a 5 second write deadline (`clientWriteTimeout`). A wedged client therefore
stalls PTY reads for up to 5s, during which the PTY buffer can fill and the
session's process blocks on its own output.

On a Unix socket this is nearly unreachable. Over a WebSocket on a laptop whose
wifi just died, it is the normal case: the socket stays writable, then stalls.
Fixing this before step 2 means never debugging it as a mystery hang later.

## Design

Give `attachment` an outbound queue and a writer goroutine:

- buffered channel of frames (start at 256 frames / ~4MB of bytes, whichever
  first — measure and pick);
- `pump` does a non-blocking send under `w.mu` and returns immediately;
- on overflow, disown the client rather than dropping frames silently: a client
  that missed bytes has an invalid sequence position, and reattaching with its
  last good `seq` is exactly the recovery path that already exists;
- the writer goroutine owns all socket writes, so `attachment.mu` and the write
  deadline dance go away;
- control frames (exit, detach) must still reach a client whose data queue is
  full — either a separate priority path or reserve capacity.

Keep the invariant that ring write and client delivery are ordered under `w.mu`;
only the *blocking* part moves out.

## Acceptance

- `go test -race ./internal/worker` with a test that attaches a client which never
  reads, writes far more than the queue bound, and asserts (a) the PTY pump keeps
  draining, (b) the client is disowned, (c) a fresh attach with a valid `seq`
  still gets correct output.
- Both existing integration scripts still pass.
- `clientWriteTimeout` is gone or justified in a comment.

## Out of scope

Flow control on the *input* direction, and any WebSocket-specific handling.
