# T09 — Interactive picker

**Status:** not started · **Blocked by:** T07 · **Owns:** `internal/tui/`

## Goal

Bare `m` opens a host and session picker: Bubble Tea v2, Bubbles v2 list/table,
Lip Gloss v2 styling. Pick a host, pick a session or start a new one, attach.

Show per session: ID, state, age, command, cwd, and whether the row is live or
cached-stale. Offline hosts appear, marked, and offer `wake` where applicable.

Reconnect animation with Harmonica is explicitly sanctioned by the plan.

## Acceptance

- `x/teatest` + `x/golden` coverage for the main flows: navigate, attach, new
  session, and the offline-host case.
- Handing off from the TUI to a raw-mode attach and back restores the terminal
  cleanly, including on error paths. Test the error paths; that is where terminal
  restoration actually breaks.
- Usable at 80x24.
