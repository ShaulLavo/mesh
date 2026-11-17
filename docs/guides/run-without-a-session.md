# Run a command that ends with its connection

Use `mesh ssh` when you want a shell or one command on a host and no saved session. It uses the host's private Mesh SSH listener on port 2222 and your existing device approval. The command ends when its CLI or connection closes. It has no detach key or recovery.

Open a shell:

```bash
mesh ssh pc
```

Run one command:

```bash
mesh ssh pc -- df -h
```

To select a working directory and override environment variables, use:

```sh
mesh ssh pc --cwd /work/repo --env CI=1 --no-pty -- go test ./...
```

An absolute `--cwd` selects that remote directory. A relative path starts at the
remote account's home directory. The default is the home directory. Mesh reports
an error if the selected directory is unavailable. Paths have no shell expansion.

Repeat `--env KEY=VALUE` for each override. Values can contain `=`. The last value
for a repeated key wins. Other variables keep the daemon's environment. Each
command has its own directory and environment.

Use `-T` or `--no-pty` to force pipe mode when your runner allocates a terminal.
Pipe mode keeps stdout and stderr separate and preserves their bytes. Closing
stdin sends EOF; the command can continue producing output. Mesh drains output
before returning the remote exit status.

Mesh passes command arguments directly, with no shell on the remote side. For pipes and other shell syntax, start a shell yourself:

```bash
mesh ssh pc -- sh -c 'df -h | head -n 2'
```

Pipe-mode calls share authenticated connections across CLI processes. Mesh starts
a private local helper for your state directory when needed. Commands use
independent SSH channels. Cancelling one command closes its channel and remote
process group; sibling commands keep running. A failed command is never replayed.
Future commands reconnect after a transport fails.
Connection setup failures have a one-second cooldown. Mesh never retries a
command whose launch may have reached the host.

The helper closes connections after one minute without commands using them and
exits after one minute without running commands. It adds no service or daemon
installation. It keeps at most eight destinations, sixteen commands per
destination, and thirty-two running commands. A ninth destination replaces the
least recently used idle connection. A command beyond any other limit, or one
that cannot reach the helper, runs on its own direct connection instead, as
interactive shells do. The host's existing admission limit also applies.

The helper confirms each request before it starts the command. If the helper
goes away before confirming, for example while it is idling out, the command
tries the helper once more and then connects directly. Nothing has run at that
point. A channel that fails to open because its shared connection has died is
opened once more on a fresh connection; no command was sent on the dead one.

Pending channel setup, including cancelled waits for a late SSH reply, counts
against those limits. An additional bound of 128 setup operations covers all
destinations and connections being cleaned up. Cancelling a pending channel open
prevents it from starting a command after a late reply. If an execution request
was already sent, its outcome can be uncertain; Mesh reports the error and never
replays it. A transport with abandoned setup closes after its last active caller
leaves, so a cancelled setup cannot retain resources indefinitely after siblings
finish.

Interactive shells retain their own SSH connection. Destination identity checks
and device grant revocation apply to both modes.

To keep a command running after you disconnect, use a normal session instead: `mesh pc -- command`. See [Getting started](../getting-started.md).
