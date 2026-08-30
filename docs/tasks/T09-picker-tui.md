# T09 — Interactive picker

**Status:** complete · **Blocked by:** T07 · **Owns:** `internal/tui/`

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

## Design choice

T09 retains three exact 80x24 sketches in
`internal/tui/testdata/TestDesignSpace.golden`.

| Design | Strength | Cost | Decision |
|---|---|---|---|
| Two panes | Hosts and sessions stay visible together. | Command and cwd fields truncate at 80 columns. | Rejected. |
| Host then session pages | Each session gets the full width for state, age, command, cwd, and cache state. | Selecting a session takes one extra Enter key. | Chosen. |
| Flat action palette | New, resume, wake, and attach share one short list. | Host boundaries and stale cache context are easy to miss. | Rejected. |

Regenerate the comparison with:

```bash
go test ./internal/tui -run '^TestDesignSpace$' -update
```

## Implementation

Bare `mesh` opens a host page, then a full-width session page. The session page
uses `n` for a new session, `r` to resume the latest active session, and `w` to
wake an offline host. Esc returns to the host page, then cancels. Session rows
show ID, state, age, command, cwd, and either `live` or `cached-stale`.

The TUI represents attach, new, resume, wake, and cancel as separate internal
types. `NewCLIPicker` converts the finished action to `cli.PickerSelection` only
after Bubble Tea returns. `cmd/mesh` adds the picker to `commandDependencies()`
without replacing T08's bootstrap function.

Bubble Tea owns the alternate screen and restores it before `Program.Run`
returns. Tests inspect the real terminal control stream on both the selection
path and a canceled-context error path. A CLI test also checks that raw attach
starts after the picker callback returns. If either stdin or stdout is not a
terminal, the picker returns `ErrNonInteractive` without starting Bubble Tea.

## New dependencies

- `charm.land/bubbletea/v2 v2.0.9` owns the event loop, resize messages, and
  alternate-screen lifecycle.
- `charm.land/bubbles/v2 v2.2.1` provides the paged host and session list.
- `charm.land/lipgloss/v2 v2.0.6` styles selection, status, and stale warnings
  without adding a second rendering stack.
- `github.com/charmbracelet/x/exp/teatest/v2` at
  `v2.0.0-20260830003929-9f48cc723c1c` drives the picker flows through the real
  Bubble Tea program API.
- `github.com/charmbracelet/x/exp/golden` at
  `v0.0.0-20260830003929-9f48cc723c1c` stores the flow snapshots and the retained
  design comparison. Both experimental packages are pinned to one revision.

`integration/picker_non_tty.sh` protects the non-terminal path with a two-second
upper bound. The picker golden tests cover attach, new, resume, wake, cancel,
resize, empty hosts, empty sessions, and the 80x24 layout.
