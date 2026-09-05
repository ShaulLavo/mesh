# Use Mesh for every terminal window

Set your terminal's command to `mesh --window` to keep its shell running after
the window closes. Mesh starts `$SHELL` in the inherited working directory when
there is nothing to resume or relaunch, with no Mesh output before the prompt.

Install `mesh` somewhere on your terminal's `PATH` before changing the command.
If the terminal cannot find it, use the executable's absolute path in the
configuration below.

## Set the terminal command

In your Ghostty configuration, set
[`command`](https://ghostty.org/docs/config/reference#command):

```ini
command = mesh --window
```

This example passes `ghostty +validate-config --config-file=<file>` with
Ghostty 1.3.1-arch2. Open a new window to use the command.

In `alacritty.toml`, set
[`terminal.shell`](https://alacritty.org/config-alacritty.html):

```toml
[terminal.shell]
program = "mesh"
args = ["--window"]
```

In `kitty.conf`, set
[`shell`](https://sw.kovidgoyal.net/kitty/conf/#opt-kitty.shell):

```conf
shell mesh --window
```

## Resume your work

Open a window to see detached sessions on this machine, with the newest selected.
The prompt reads local session files and Unix sockets without waiting for the
daemon's session list or a remote host.

- Press `enter` to resume the selected session, or `1` through `9` to pick a row.
- Press `n` to start a fresh shell.
- Press `l` to open the full picker, with this machine first and remote hosts below.
- Select an interrupted session and press `enter` to recover a shell in its saved
	directory, or reconnect to its exact saved remote target. Saved previews read
	**Previous output** with the checkpoint time. Press `s` to request a local
	shell instead of the remote target. Press `x` to forget an interrupted session.
- Press `l` for the full picker. Press `c` there to explicitly restart the recorded
	command. Exited sessions and previous attempts remain in the full picker.

Attached sessions appear last and are never selected automatically. To use one,
open the full picker and choose its explicit `take over` action.

To resume without a prompt, add `--take` to your terminal command:

```bash
mesh --window --take
```

Each window claims a detached session without taking it from another window.
If none is available, Mesh starts a fresh shell. `--take` skips old workers that
cannot claim sessions safely. It does not restart interrupted or exited work,
or follow saved remote targets. To attach to a legacy local session explicitly,
run `mesh attach ID`. For a remote session, run `mesh ID`. These explicit
commands can take the session from an existing client.

## Save shell directories and history

For Bash, add this line to `~/.bashrc`:

```bash
source <(mesh shell-init bash)
```

For Zsh, add this line to `~/.zshrc`:

```zsh
source <(mesh shell-init zsh)
```

The opt-in hook reports the interactive shell's directory after each prompt and
appends completed commands through the shell's normal history mechanism. It
preserves existing prompt hooks and history exclusions. Repeated sourcing does
not duplicate the hook. Outside Mesh, the helper stays silent.

Without the hook, recovery uses the last observed directory, then the launch
directory. The picker labels these fallbacks. An unsubmitted command line is not
saved. History appends and periodic checkpoints do not guarantee that the final
commands survive a power cut.

For recovery commands, saved restart recipes, and retained attempts, see
[workspace recovery](recovery.md).

## Leave a session running

Press `ctrl+]` to detach the innermost session. For example, after `mesh pc`
inside a Mesh window, `ctrl+]` returns to the local shell.

While a nested client is registered, press `ctrl+^` to leave the whole window.
The local shell and its remote attachment keep running. Closing or crashing the
terminal also leaves those sessions running. Open another window to resume the
local session and return to the remote screen. A row labelled `on pc/7K3D`
identifies the session nested inside it.

To change the leave-all key, add `--leave-key`, for example:

```bash
mesh --window --leave-key 'ctrl+b'
```

Use `--leave-key none` to disable leave-all. Use `--detach-key 'ctrl+a'` to choose
an explicit detach key. Use `--raw` to pass every byte through that client.

Inside an older worker, follow the printed detach hint. Those workers retain
the depth-based keys: outermost `ctrl+]`, then `ctrl+^`, then `ctrl+_`. Leave-all
stays inactive without a registered inner client, so a legacy inner client can
still receive `ctrl+^`. Further sessions nested through that older worker also
follow the printed depth-based hints. Existing workers keep their version
across daemon upgrades; new sessions use the updated behavior.

An older client cannot take over a session whose active inner clients use the
shared detach key. Use a current client outside the older nesting chain, or
detach those inner sessions first.

## Use a shell startup guard as a fallback

If your terminal cannot set a command, configure it to open an interactive login
shell. Add the matching guard below to your shell startup file.

For Bash, add this to `~/.bashrc` and ensure your login startup file sources it:

```bash
if [[ $- == *i* && -z ${MESH_SESSION_ID-} ]] && shopt -q login_shell; then
	exec mesh --window
fi
```

For Zsh, add this to `~/.zshrc`:

```zsh
if [[ -o interactive && -o login && -z ${MESH_SESSION_ID-} ]]; then
	exec mesh --window
fi
```

These guards skip scripts, non-login shells, and shells already inside Mesh.
If you run `mesh --window` inside a session directly, Mesh prints one line and
starts a plain shell to avoid accidental nesting.
