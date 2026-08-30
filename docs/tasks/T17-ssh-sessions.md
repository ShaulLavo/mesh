# T17 — Sessions over SSH

**Status:** not started · **Blocked by:** T09, T15 · **Owns:** `internal/sshd/session.go`

## Goal

```
ssh pc.mesh.shaulavo.dev              # picker, then attach
ssh pc.mesh.shaulavo.dev -t 7K3D      # straight into one session
ssh pc.mesh.shaulavo.dev ls           # one-shot, scriptable, no PTY
```

A phone with the Tailscale app and Termius becomes a complete Mesh client (D21).
The examples assume the client selected an explicitly authorized key. T15
documents the stock OpenSSH form; other clients import the same credential.

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

`cli.Attach` now stops SIGWINCH delivery, cancels its input reader, closes the
transport, and waits for both relay goroutines before it returns. Keep that
return barrier when adapting it for SSH.

An SSH session does not use `os.Stdin`, `os.Stdout`, or process SIGWINCH.
Parameterize the attach path with its input, output, initial terminal size,
window-change channel, and an input-cancel function. The local CLI adapter keeps
the existing terminal behavior. The SSH adapter must unblock its reader when
the connection closes. Neither adapter may return while an input or resize
goroutine is still running.

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
