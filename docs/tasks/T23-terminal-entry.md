# T23 — Mesh as your terminal

**Status:** complete · **Prerequisites:** T09, T22 · **Owns:**
`internal/cli/window.go`, `internal/worker/nesting.go`, `internal/tui/` (local
section, compact mode), `internal/protocol/` (nesting messages),
`docs/terminals.md`

## Goal

Every terminal window is a Mesh session, whether or not a remote host is ever
involved. Ghostty runs `mesh --window` instead of a shell. A window that opens
onto nothing to resume starts a shell instantly. A window that opens while
detached sessions exist asks, in one line each, which one to take back.

```bash
mesh --window          # the terminal's command; prompt or fresh shell
mesh --window --take   # newest detached session without asking; for keybinds
mesh                   # the full picker, this host first, remotes below
```

Inside such a window, `m pc` nests a remote session in the local one exactly as
it does today. That nesting is the main use case, not an edge case, so the
detach keys and the picker have to describe it honestly.

The invariants hold unchanged. Closing the window, crashing the terminal, or
losing wifi with a remote hop inside leaves everything running. A reboot still
reports `interrupted` and is offered a fresh start, never a resurrection.

## What already exists

- `mesh local` creates and attaches a session on this host with or without the
  daemon, and `mesh local -r` resumes the newest live one. The window path
  shares creation and attachment but resumes only detached sessions.
- Every session exports `MESH_SESSION_ID`, `MESH_HOST_ID`, and `MESH_DEPTH`.
  `worker.ContainingSessionWorker` finds the worker above the calling process
  by walking the process tree, and T22's `session.containment` returns the
  exact outer path. A client can therefore always tell whether it is already
  inside a session, which is what stops `mesh --window` from recursing when a
  terminal is opened from inside another one.
- Client death is already survived and tested (`survives_client_death.sh`).
  The whole window model rests on that script.
- The picker labels rows by project directory and inspects the selected
  session's live screen. The startup prompt is that data in a compact layout.
- Nested detach keys exist as a fixed table by depth (`detachKeysByDepth`),
  with the outermost client owning ctrl+]. D27 reverses that ownership.

## How nesting behaves under the hood

This section exists because the question will be asked again.

Session A runs on the laptop; its worker owns the PTY running your shell. When
you type `m pc` in that shell, the `mesh` client process is a child of A's
shell. It creates session B on pc, whose record lives only in pc's SQLite. The
laptop stores nothing about B except the stale-row catalog cache. There are two
sessions on two hosts and they are different things: A is a laptop shell that
happens to be running a Mesh client, B is a pc shell. Nothing is duplicated and
nothing needs deduplication (invariant 6).

Because the remote client lives inside A's PTY, the window dying does not even
disconnect from pc. The next window attaches A and the screen is pc's shell.

| Event | A (laptop) | B (pc) | Next window |
|---|---|---|---|
| terminal crashes | detached, inner client still attached to B | untouched | attach A, back in pc's shell |
| laptop loses wifi | untouched | inner client reconnects | nothing to do |
| laptop reboots | interrupted | detached | relaunch A, or take B directly |
| pc reboots | fine, inner client reports the loss | interrupted | A's shell |

Output from B crosses two replay rings and two terminal emulators. One extra
Unix socket hop is not measurable; resize propagation through the nested client
is the part that needs a script.

## Responsibilities

1. **Nesting registration in the worker.** Before a client attaches to a
   session, it opens one extra connection to its immediate containing worker
   (the one `ContainingSessionWorker` already locates) and sends
   `session.nest` carrying the identity it is about to attach to. The worker
   holds that connection for the life of the inner attachment. Open means
   nested; closed for any reason, including the inner client crashing, means
   not nested. The worker pushes `session.nesting` to its current attacher
   whenever the set changes, and `session.attached` carries the current set so
   a client that steals A learns the state immediately.

   Registration happens before the inner attach, so the window in which ctrl+]
   could still reach the outer client is the attach round trip, not the
   registration round trip. If registration fails because the containing
   worker predates it or its attacher cannot forward the shared detach key,
   the client falls back to the depth-keyed table and prints the existing hint.
   That fallback propagates through further nested sessions.

2. **Detach keys per D27.** A client with a registered inner attachment passes
   ctrl+] through. Without one, ctrl+] detaches, as today. ctrl+^ is consumed
   by the outermost client only while its worker reports a registered inner
   attachment; it
   detaches that client and, because every inner client lives inside that
   PTY, leaves them all in one keystroke. `--detach-key` and `--raw` keep their
   meaning; `--leave-key` names the leave-all key and accepts `none`.

3. **`mesh --window`.** The terminal's entry point. It refuses without a TTY,
   the same way the picker does, and behaves as follows inside a TTY:

   - Already inside a session (containment discovery succeeds): print one line
     saying so and start a plain shell. Never nest by accident.
   - No detached sessions on this host: create a session running `$SHELL` in
     the inherited working directory and attach, with no output at all before
     the shell prompt. This is the common case and it must feel like opening a
     terminal.
   - One or more detached sessions: the compact prompt. Each row is the picker
     row: project label, ID, `on pc/B` when something is nested inside, last
     output age. The newest is preselected and a few preview lines show under
     it. `enter` resumes, a digit picks another, `n` starts fresh, `l` opens
     the full picker, `x` forgets an interrupted session. Attached sessions are
     listed last, are never preselected, and need the picker's existing
     "take over" path (D28).
   - `--take` resumes the newest detached session without the prompt, and
     starts fresh when there is none. A Hyprland keybind that spawns three
     windows with `--take` restores three sessions.
   - Interrupted sessions appear below detached ones and `enter` on one starts
     a new session with the same command and working directory, then removes
     the old record. They stay listed until relaunched or forgotten.

   The prompt reads session directories and dials Unix sockets only. It never
   touches the daemon socket for listing, never queries a remote host, and must
   render within the time a shell takes to print its prompt.

4. **This host in the picker.** The picker gets a first section for this
   machine, populated from `localSessionRows` before any remote query starts,
   labelled the way `mesh ls` already labels it. Remote hosts load underneath
   as they do now. Rows for local sessions show `on pc/B` from the nesting set;
   rows for remote sessions viewed through a local one show `via A` from the
   containment path T22 already records. Selecting the remote one keeps the
   `enter take over` footer because D3 will evict the inner client.

   Only local containing screens are captured before rendering. Remote
   containing identities remain marked unavailable for that picker instance.
   Waiting for their screen would delay local startup, and capturing it after
   rendering could record the picker itself. Other remote session inspections
   still load on selection.

5. **Relaunch for interrupted sessions.** One code path used by both the prompt
   and the picker: create with the recorded command and cwd, attach from
   sequence zero, remove the old record once the new session is answering.
   Applies to every host, through the daemon, with the same rules.

6. **Terminal configuration.** `docs/terminals.md` with a verified Ghostty
   snippet (`command = mesh --window`) and documented Alacritty and Kitty
   equivalents. A shell-rc guard is documented as the fallback for terminals
   that cannot set a command; it must check `MESH_SESSION_ID` and only run in
   an interactive login shell.

## Protocol

Additive control messages; older receivers ignore fields they do not know.

- `session.nest` — request from an inner client to its containing worker,
  carrying the `SessionIdentity` being attached. The worker keeps the
  connection open and treats its closure as the end of nesting. At most 32
  registrations per worker, matching the containment path bound.
- `session.nesting` — pushed by a worker to its attacher with the current set
  of nested identities. `session.attached` gains the same `Nested` field.
- `session.inspected` gains `Nested`, so inspection clients can render
  `on pc/B` without attaching.
- `session.attach-detached` claims a worker only when it has no attacher, under
  the attachment lock and before resizing. A separate request type makes old
  workers reject the operation instead of ignoring an optional field and
  stealing. `--take`, fresh windows, and automatic picker resumes use it.
- In worker responses, `NestingSupported` distinguishes an empty nesting set
  from an unsupported registration. On attachment requests, it says the client
  and its upstream chain can forward the shared detach key. A worker refuses
  registration when its attacher lacks that capability. While registrations
  exist, it rejects incompatible attachments before takeover or resize, even
  when the worker is currently detached.

No table changes. Nesting is live state that dies with the worker and is
recovered from the connections, never from SQLite.

Interrupted records are excluded from automatic retention. Forgetting one uses
the daemon when available. Otherwise a `.forgotten` marker hides it locally
until the next daemon reconciliation retires its database row and files.
Reconciliation removes the row before the files and removes the marker last,
so interrupted cleanup can be retried.

## Version and rollout behavior

- A new client inside a legacy worker cannot register. It uses the depth table
  and the existing hint, and the outer client keeps ctrl+] because its
  `session.attached` carries no `Nested` field. Leave-all is inactive there,
  which is safe: ctrl+^ then passes through to the depth-1 client as today.
- A new worker with a legacy inner client sees no registration and behaves
  as before. Leave-all stays inactive so the legacy inner client receives its
  depth-based detach key.
- A current worker reached through a legacy or fallback client also uses the
  depth table for its inner clients. Successful registration with one current
  worker alone cannot establish that every outer client forwards `ctrl+]`.
- A legacy or fallback client cannot take over a worker with active dynamic
  registrations. Close those inner attachments or use a current client from
  outside that legacy chain. Clients reject attachment to a known containing
  session before connecting, including when the target worker predates the
  containment protocol.
- `--take` skips workers without detached-only claims. Explicit `mesh attach
  ID` locally or `mesh ID` remotely still supports those workers with the usual
  takeover behavior.
- The installers keep session workers alive across a daemon restart, so the
  first window opened after upgrading is a new worker with the new behavior,
  and old windows keep their old keys until their sessions end.

## Acceptance

- Opening a window with nothing to resume shows a shell prompt with no Mesh
  output before it.
- Killing the terminal with a remote hop inside leaves both sessions alive;
  the next `mesh --window` prompt shows A labelled with pc's session, and
  `enter` shows pc's screen.
- With A containing B, ctrl+] detaches B and returns to A's shell; ctrl+^ from
  the same position ends the window with both sessions still running and B
  still attached inside A.
- Without nesting, ctrl+] detaches as today; `mesh local` is unchanged.
- Resizing the window while attached through A to B resizes B's PTY.
- A second window never steals the first window's session unless "take over"
  is chosen explicitly.
- `mesh --window` from inside a session starts a plain shell and says why.
- The prompt and picker render every state at 80×24.
- All existing integration scripts pass unchanged.

## Verification

New integration scripts, each with its own state directory:

- `nested_detach_keys.sh` — ctrl+] reaches the inner client, ctrl+^ leaves all.
- `nested_resize.sh` — SIGWINCH propagates through the inner client.
- `window_death_keeps_remote.sh` — kill the outer client, verify the inner
  attachment survives and the next attach shows the remote screen.
- `window_entry.sh` — non-TTY refusal, `--take` with and without a detached
  session, and no output before the shell when nothing is detached.
- `window_relaunch.sh` — recorded command and directory, online and offline
  retirement, failed-launch preservation, and explicit forgetting.

`scripts/check-t23.sh` runs the focused race tests, vet, these five real-PTY
scenarios, and CGO-disabled Linux amd64/arm64 and Darwin arm64 builds.

The final review adds regressions for capability propagation, fallback-key
ownership, rejection before takeover or resize, and self-attachment validation.
Real PTYs with an older outer worker and two current inner workers confirm the
three depth keys still reach their respective clients. Direct `mesh attach`
and `mesh local --resume` both reject the containing older session and leave
its shell usable.

Then the full set:

```bash
gofmt -w <changed Go files>
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/verify.sh
```

## Out of scope

- A general config file. `--window`, `--take`, and `--leave-key` are flags
  because the terminal config is where they belong; revisit if a third flag
  arrives.
- Restoring session contents after a reboot (invariant 5).
- Mirroring one session in two windows (D3).
- Restoring window layout. Hyprland owns layout; Mesh owns sessions.
