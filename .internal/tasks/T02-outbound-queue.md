# T02 — Bounded outbound queue per attachment

**Status:** complete · **Blocked by:** nothing · **Owns:** `internal/worker/worker.go`,
`internal/worker/serve.go`, `cmd/mesh/main.go`

## Goal

The PTY reader must never block on a slow or dead client.

## Previous behaviour

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
- the writer goroutine owns all socket writes, so `attachment.mu` goes away and
  socket deadlines cannot delay the PTY reader;
- control frames (exit, detach) must still reach a client whose data queue is
  full — either a separate priority path or reserve capacity.

Keep the invariant that ring write and client delivery are ordered under `w.mu`;
only the *blocking* part moves out.

## Implementation

Each attachment has one writer goroutine and a FIFO queue. The queue accepts at
most 256 data frames and 4 MiB plus one 32 KiB live frame, whichever limit it
reaches first. The extra frame prevents a full replay from immediately
disowning a healthy client when new PTY output arrives. Eight reserved slots
admit attach, detach, and exit controls after the data limit. Replays split into
32 KiB frames, which keeps a full 4 MiB replay below `protocol.MaxPayload` per
frame.

`Worker.pump` copies each reusable PTY buffer into the queue while it holds
`w.mu`. A full queue disowns and closes that attachment. The PTY reader then
continues to write to the replay ring. A new client resumes from its last valid
absolute sequence.

Socket writes retain a five-second deadline. Only the attachment writer waits on
that deadline, so a stuck socket cannot delay the PTY reader.

On process exit, the worker closes its retained copy of the PTY slave and gives
the pump 250 ms to consume buffered output. The worker closes the PTY after that
limit so an inherited slave descriptor cannot keep the session alive. The same
worker lock orders this cutoff before the exit control, so the pump cannot queue
data behind the exit.

Once exit starts, the worker rejects new attachments. `Run` waits up to five
seconds for every current or displaced attachment writer. After that limit, it
closes any remaining connections before the worker process ends.

The detached session worker also waits up to five seconds for its first real
attachment when a command exits before the spawn readiness probe completes. A
new `mesh local` attachment requests output from sequence zero; explicit and
resume attachments retain the normal bounded-tail behavior.

## Acceptance

- `go test -race ./internal/worker` with a test that attaches a client which never
  reads, writes far more than the queue bound, and asserts (a) the PTY pump keeps
  draining, (b) the client is disowned, (c) a fresh attach with a valid `seq`
  still gets correct output.
- All integration scripts pass.
- `clientWriteTimeout` is gone or justified in a comment.

Verified with:

```bash
go test -race -count=50 ./internal/worker
go test -race -count=1 ./...
go vet ./...
go build ./cmd/mesh
./integration/survives_client_death.sh
./integration/detach_and_steal.sh
./integration/flushes_on_exit.sh
```

## Out of scope

Flow control on the *input* direction, and any WebSocket-specific handling.
