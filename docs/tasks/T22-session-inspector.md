# T22 — Live session inspector

**Status:** complete · **Blocked by:** T09 · **Owns:** `internal/terminal/`,
`internal/worker/`, `internal/protocol/`, `internal/daemon/`, `internal/cli/`,
`internal/tui/`

## Goal

The session picker must answer what a session contains before an attach steals
it from another client. The highlighted session shows its host, current working
directory when Mesh can observe it, foreground command or terminal title, last
output time, attachment state, and a bounded view of its current screen. The
preview preserves safe terminal colors and non-animated text styles. If the
picker runs through nested Mesh attachments, it can show the last screen from
before the picker began writing for every enclosing Mesh terminal.

The list stays stable while inspection data refreshes. At 80 columns, the list
and the selected-session inspector stack vertically. A wider terminal does not
switch to a separate layout.

## Data model

Durable catalog data and live inspection data remain separate.

- `SessionInfo.Cwd` is the launch directory. The UI calls it `started in`.
  When no directory was configured, the worker records the directory it
  actually inherited instead of leaving this fact blank.
- A live `SessionInspection` contains an observation time, an optional current
  directory and its source, an optional foreground command, an optional
  terminal title, the last output time, whether a client is attached, bounded
  plain preview lines, and optional structured style runs for those lines.
- Current-directory sources are `process` and `terminal`. `terminal` means the
  session emitted OSC 7. If neither source is available, the UI shows the
  launch directory as `started in` and does not call it current.
- Process life, attachment ownership, and recent output are separate facts.
  The UI does not use `running` as a synonym for activity.

`session.inspect` is a one-shot control request. It never attaches, resizes the
PTY, changes attachment ownership, or writes session data. The request bounds
the preview width and height. The response contains plain terminal text, not
raw control sequences. Optional style runs describe colors, emphasis, and
underline presentation as typed values.

Terminal containment is explicit attachment state:

- `SessionIdentity` combines a host ID and session ID because session IDs are
  host-local.
- Every attach carries its known `ContainingSessions` path, ordered from the
  client's immediate Mesh session outward. The destination worker validates
  and stores that path on the attachment before it changes ownership.
- A local `session.containment` request returns the answering worker's identity
  followed by the path stored on its current attachment. For example, a picker
  in session B, attached through A inside R, receives `B, A, R`.

The CLI uses a bounded operating-system parent walk only to locate the immediate
worker and its socket. The outer path comes from the workers' attachment state;
Mesh does not infer it from a title, command, or host-local session ID.

## Picker behavior

The session screen keeps compact rows laid out as aligned columns. Each row
leads with the terminal title when the host reports one, followed by a quieter
project label derived from the best available path and command; a session
without a title shows the label alone. Fixed columns then show attachment state,
last output age (or age since start when the session has not printed), and the
session ID. The selected row expands into a full-width inspector with the path,
foreground command, launch command, activity, and current-screen preview.

Changing the selection starts an asynchronous inspection. Mesh ignores a late
response for a session that is no longer selected. The selected session
refreshes every two seconds. Visible row summaries refresh every four seconds,
with at most three summary requests in flight.

Before the TUI writes its first frame, the CLI discovers the complete available
containment path and concurrently attempts to inspect every exact host/session
identity in that path. Those terminals all mirror the picker's output, so the
picker keeps each successful capture frozen and never live-inspects any of them
after it becomes visible. If an identity is proven but its screen cannot be
captured, the panel says so instead of inspecting the rendered picker. Other
sessions and all catalog data continue to refresh.

`Space` toggles a full-height preview. `Enter` attaches. If another client is
attached, the footer says `enter take over` because D3 evicts that client.

Offline hosts keep their cached catalog rows. They do not show cached terminal
content. The inspector states that a live preview is unavailable.

`mesh ls` adds a `STARTED IN` column. Until the non-interactive command gains
live inspection fan-out, it shows the launch directory and cannot be mistaken
for the current path.

## Limits and safety

- A request asks for at most 160 columns and 24 rows.
- A response contains at most 24 lines, 160 display columns per line, and 32 KiB
  across title, path, command, and preview text.
- A styled response contains at most 3,840 runs. Each run must reproduce the
  corresponding plain line when its text is concatenated.
- A containment path contains at most 32 unique host/session identities. Attach
  rejects malformed, duplicate, cyclic, or over-depth paths before displacing
  the current attachment.
- The protocol parser rejects unknown directory sources, invalid times,
  oversized text, invalid colors or underline styles, mismatched style runs,
  and responses for the wrong session.
- Style runs contain only control-free text; basic, indexed, or RGB foreground,
  background, and underline colors; bold, faint, italic, reverse, and
  strikethrough flags; and a bounded underline shape. They cannot contain raw
  terminal controls, links, blinking, or concealment. Concealed cells become
  spaces of the same width.
- The protocol boundary rejects structured runs that do not reproduce the plain
  line. The TUI checks the relationship again and falls back to escaped plain
  text when style data is absent or inconsistent.
- The TUI applies its existing terminal-text escaping to every remote string.
- Inspection failures stay inside the selected-session panel. They do not close
  the picker.

## Version and rollout behavior

`ContainingSessions` and `StyledPreview` are additive protocol fields. An older
receiver ignores a field it does not know.

- The daemon relay forwards an attach frame unchanged. A chain-aware client can
  therefore send `ContainingSessions` through a daemon that predates that field;
  the destination worker either records the path or, if it also predates the
  field, ignores it without changing attach behavior.
- Live inspection requires both the daemon and worker to support
  `session.inspect`. A session whose worker predates inspection reports that its
  live view is unavailable; the request still cannot attach or disturb it.
- An inspection-capable worker that does not emit `StyledPreview` remains
  usable. The daemon can replay that worker's bounded raw ANSI output at the
  live PTY dimensions, but it accepts the recovered presentation only when
  every replayed row reproduces the worker's authoritative plain inspection.
  The client still receives typed style runs rather than raw controls. If the
  replay is incomplete or does not match, the client renders escaped plain text.
- A new client inside a legacy worker can prove the immediate local
  host/session identity from the ancestor-located worker directory and the local
  host identity. It cannot reconstruct outer attachment links that older
  workers or clients never recorded, so mixed-version paths are a proven prefix,
  never a guess.
- The installers replace the Mesh binary and restart the daemon without
  terminating session-worker processes (`KillMode=process` on Linux and
  `AbandonProcessGroup` on Darwin). A running worker keeps the feature set of
  the executable that started it. The daemon's verified ANSI compatibility path
  can add typed presentation to an older plain inspection without restarting
  that worker; all other new worker behavior still requires a recreated session.
  New sessions use the installed binary directly.

## Acceptance

- A worker inspection returns a cropped current-screen preview without taking
  ownership from an attached client.
- OSC 0 and OSC 2 update the terminal title. OSC 7 updates the reported current
  directory.
- Linux resolves a live member of the PTY foreground process group for the
  current directory and command, with the session leader as a fallback. Darwin
  observes the same process group with bounded system tools, then falls back to
  OSC data.
- The remote path works through the daemon and validates every new wire field.
- Moving between rows cannot display a late preview under the wrong session.
- Attach propagates an exact immediate-to-outer containment path, and a worker
  query prepends that worker without permitting a duplicate or cycle.
- A picker opened through nested Mesh sessions shows the available pre-picker
  capture for every containing terminal and cannot feed a rendered picker frame
  into any of them.
- Terminal colors and text styles survive inspection without carrying raw ANSI
  sequences across the protocol boundary.
- The normal, loading, unavailable, stale, and full-preview states fit 80×24.
- Existing attach, kill, remove, wake, terminal restore, and non-TTY behavior
  remain unchanged.

## Verification

Run the focused checks after each layer:

```bash
./scripts/check-t22.sh
```

The retained integration regression crosses a real WebSocket, daemon
lifecycle, Unix worker socket, process observer, and terminal emulator:

```bash
./integration/session_inspection.sh
```

Run the complete repository checks before completion:

```bash
gofmt -w <changed Go files>
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/verify.sh
```
