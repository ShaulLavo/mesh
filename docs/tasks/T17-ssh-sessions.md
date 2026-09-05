# T17 — Sessions over SSH

**Status:** complete · **Prerequisites:** T09, T15 · **Owns:** `internal/sshd/session.go`,
`internal/cli/ssh.go`, shared attachment, and terminal picker adapters

## Goal

```
ssh pc.mesh.shaulavo.dev              # picker, then attach
ssh -t pc.mesh.shaulavo.dev 7K3D      # straight into one session
ssh pc.mesh.shaulavo.dev ls           # one-shot, scriptable, no PTY
```

These commands assume the D24 client configuration sets `Port 2222`.

A phone with the Tailscale app and Termius becomes a complete Mesh client (D21).
The examples assume the client selected an explicitly authorized key. T15
documents the stock OpenSSH form; other clients import the same credential.

## Responsibilities

1. **Render the picker through Wish.** T09's Bubble Tea v2 model, served by the
   shared terminal picker adapter inside the Wish handler. If T09's model cannot be
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

`cli.Attach` manages local raw mode, SIGWINCH, and nesting discovery.
`cli.AttachWithTerminal` runs the shared attachment using explicit streams,
dimensions, resize events, and cancellation. Both paths cancel input, close the
transport, and join their relay work before returning.

An SSH channel owns a bounded pipe and a continuously drained resize channel.
The picker and attachment take turns reading the pipe. Canceling one reader
preserves the SSH channel so detach can return to the picker. Closing the
channel cancels its application and joins both channel pumps. Input EOF also
closes that channel, releasing application writes when the client has stopped
reading output.

## Implementation

`cmd/mesh` supplies the session-handler factory through `cli.Dependencies` and
`daemon.Config`. The daemon binds that factory to its normalized state directory
and passes the handler to each SSH listener. This avoids the import cycle
`sshd -> tui -> cli -> daemon -> sshd` without moving existing client contracts.
The alternative was a shared client package containing the attachment engine,
picker types, and catalog operations. Injection requires fewer caller changes.

The SSH application queries only its destination daemon. It supports the local
picker, new sessions, detached-session resume, explicit takeover, inspection,
kill, removal, and interrupted-session relaunch. New shells use the host account's
home directory and the SSH client's TERM. SSH never executes arbitrary command
strings or forwards terminal traffic through another Mesh host.

The pinned Wish 1.4.7 `bubbletea` middleware accepts Bubble Tea v1 models.
Mesh's picker uses v2. `tui.NewTerminalPicker` supplies explicit IO, TERM, initial
size, and joined resize handling to the existing v2 model. No dependency changes
are required. A cancellable pipe reader also covers Bubble Tea's cancellation
path, which skips its normal input-reader wait.

The SSH adapter gives each channel a separate cancellation context because the
pinned SSH library's context belongs to the connection. Closing one channel
leaves sibling channels usable. Resize handling begins at the PTY request,
before shell startup, so queued window changes cannot block the shell request.
Terminal sizes are limited to 2048 per axis and 256K cells. TERM is limited to
64 ASCII letters, digits, or `-+._`. The adapter preserves terminal output bytes.

Bare interactive SSH opens the picker. A single session ID attaches directly,
and `ls` prints the destination host's catalog without a PTY. Invalid commands
exit nonzero. `hello` remains available as an authentication health check.

## Verification

Verified 2026-09-05 with `go test -race ./...`, `go vet ./...`, clean module and
formatting checks, all 27 integration scripts, and CGO-disabled Linux amd64,
Linux arm64, and Darwin arm64 builds. Change-scoped checks pass all seven linters.
Full lint reports the same eleven existing bootstrap, installer, and tag findings.

`scripts/check-t17.sh` retains the focused package checks, both SSH integration
scripts, and the three cross-builds. The stock OpenSSH scenario exercises the
shipped daemon, real PTYs, local-to-SSH takeover, repeated detach-to-picker,
resize, a killed SSH client, the original shell PID after reconnection, new
session TERM, and process exit status. SSH tests also cover input EOF during
output backpressure, sibling channel isolation, terminal bounds, and 100
connect/disconnect cycles without retained adapter goroutines.

## Acceptance

- Wish `testsession` tests: picker renders, a session attaches, output flows.
- Detach with ctrl+] returns to the picker, and the session is still running
  afterwards.
- Killing the SSH connection mid-session leaves its process alive. The worker
  becomes detached, and the daemon reports that state after reconciliation.
  Assert process survival with an integration script.
- Steal works across doors: attach locally, attach again over SSH, the local one
  is told `stolen`.
- `ssh -p 2222 host ls` returns the session list and exit code 0 with no PTY
  allocated.
- No goroutine leak across 100 connect/disconnect cycles.

## Out of scope

Files (T16), tunnels (T18), and any picker feature T09 did not build.
