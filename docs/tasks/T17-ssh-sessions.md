# T17 — Sessions over SSH

**Status:** not started · **Blocked by:** T09, T15 · **Owns:** `internal/sshd/session.go`

## Goal

```
ssh pc.mesh.shaulavo.dev              # picker, then attach
ssh pc.mesh.shaulavo.dev -t 7K3D      # straight into one session
ssh pc.mesh.shaulavo.dev ls           # one-shot, scriptable, no PTY
```

A phone with the Tailscale app and Termius becomes a complete Mesh client (D21).

## Responsibilities

1. **Render the picker through Wish.** T09's Bubble Tea model, served by Wish's
   `bubbletea` middleware. Do not fork the picker. If T09's model cannot be
   driven from a `ssh.Session`'s PTY, terminal size and window-change channel,
   that is a coupling bug in T09 worth fixing there.
2. **Attach.** Selecting a session runs the same attachment `cli.Attach` runs,
   against the same worker, with the same replay, steal and resize behaviour.
   An SSH client is just another client.
3. **Resize.** Forward the SSH window-change channel to `terminal.resize`.
4. **Detach.** ctrl+] detaches and leaves the session running, exactly as D4
   says. Dropping the SSH connection is a disconnect, not a detach, and must not
   kill anything.
5. **One-shot exec.** A command with no PTY runs a small non-interactive
   surface (`ls` first) and exits with a real status code, so it is scriptable
   from a machine without Mesh.

## The part that needs care

**`cli.Attach` currently leaks.** Its SIGWINCH goroutine ranges over a channel
nobody closes, and `relayInput` keeps reading stdin after `Attach` returns. In a
one-shot CLI process that does not matter. Here the process is a long-lived
daemon serving many connections, and every attach leaks a goroutine holding a
reader. Fix that before this task, not during it.

Also: an SSH session's stdin is not `os.Stdin`. Anything in the attach path that
reaches for the process's own terminal has to be parameterised first.

## Acceptance

- Wish `testsession` tests: picker renders, a session attaches, output flows.
- Detach with ctrl+] returns to the picker, and the session is still running
  afterwards.
- Killing the SSH connection mid-session leaves the session `running`. Assert it
  with an integration script, not a unit test.
- Steal works across doors: attach locally, attach again over SSH, the local one
  is told `stolen`.
- `ssh host ls` returns the session list and exit code 0 with no PTY allocated.
- No goroutine leak across 100 connect/disconnect cycles.

## Out of scope

Files (T16), tunnels (T18), and any picker feature T09 did not build.
