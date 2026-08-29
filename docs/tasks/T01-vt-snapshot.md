# T01 — Rendered screen snapshot on reattach

**Status:** complete · **Blocked by:** nothing · **Owns:** `internal/terminal/`,
`internal/worker/{worker,serve}.go`, `internal/cli/attach.go`,
`integration/reattach_snapshot.sh`

## Goal

When a client reattaches, it should see the terminal *as it is*, not a replay of
recent bytes. Today `serve.go` sends the last 64KB of raw output; reattaching to a
session running vim, htop, or a TUI gives you a scrolling wall of escape sequences
and a wrong-looking screen.

## Why it matters

This is the difference between "resumable" and "resumable if you were only running
`make`". It is also the fallback path for step 2: when a client has been offline
long enough to fall out of the replay window, a snapshot is the only correct answer.

## Design

Add `internal/terminal`, wrapping `github.com/charmbracelet/x/vt` behind our own
interface so its API churn stays contained (see `CLAUDE.md`).

```go
package terminal

// Screen tracks terminal state by consuming PTY output.
type Screen interface {
    io.Writer                  // fed every byte the PTY emits
    Resize(cols, rows int)
    // Snapshot renders the current screen as the escape sequences needed to
    // reproduce it on a fresh terminal: clear, cursor position, attributes,
    // alternate-screen state.
    Snapshot() []byte
}

func NewScreen(cols, rows int) Screen
```

In `internal/worker`:

- the PTY pump writes every chunk to the `Screen` as well as the ring, inside the
  existing `w.mu` critical section (that lock is what makes replay and live
  output join up exactly — do not move the write outside it);
- `Worker.Resize` forwards to the screen;
- in `serve.go`, when `ring.Since(want)` returns `ok == false`, send
  `Snapshot()` as a `KindSnapshot` frame and set `Control.Snapshot = true`;
- when a client attaches with no `LastSeq`, prefer the snapshot over the current
  bounded-tail behaviour. The `Tail` field stays for `mesh logs`-style uses.

## Acceptance

- New `integration/reattach_snapshot.sh`: start a session, run something that
  paints the alternate screen (`vim` or a `tput`-driven paint), detach, reattach,
  and assert the client received an alternate-screen restore rather than a raw
  replay.
- A Go test in `internal/terminal` asserting that feeding output into a Screen and
  replaying `Snapshot()` into a second Screen produces identical cell contents.
- Existing integration scripts still pass.

## Out of scope

Scrollback *history* rendering, mouse mode restoration, and reflow on resize
between detach and reattach. Note whatever you skip at the bottom of this file.

## Notes

Pin `x/vt` to an exact version in `go.mod`. If `x/vt` cannot report enough state
to rebuild the screen, say so here rather than working around it silently — the
fallback is to keep raw replay and mark the gap explicitly to the user.

Implemented with `github.com/charmbracelet/x/vt` pinned to
`v0.0.0-20260828171018-3c30eef5e73e`. The wrapper restores the active screen,
cell contents and attributes, cursor position/style/visibility, and alternate
screen state. Snapshot bytes have their own protocol kind, so repainting does
not invent PTY sequence offsets. Each snapshot is one bounded frame, and the
client commits its announced resume offset only after receiving that frame in
full.

Verified with terminal equivalence tests, worker ordering and resume tests, a
truncated-snapshot client test, the race detector, and
`integration/reattach_snapshot.sh` alongside all existing integrations.

Still out of scope: scrollback history, mouse and other private terminal modes,
palette/default-color restoration, and semantic reflow after resize. A snapshot
must also fit in one protocol payload (4 MiB including its session header);
oversized terminal dimensions can therefore make an attachment fail.
