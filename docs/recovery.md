# Recover a workspace

Select an interrupted session in the picker to open a shell in its saved
directory. An interrupted program shows **Open shell**. Its recorded command runs
only when you select **Restart command** with `c`.

Saved previews read **Previous output** and include the checkpoint time. They are
text for inspection. Mesh does not replay them as input or mix them into the new
terminal's output. To read all retained text, including lines outside the picker
preview, run:

```bash
mesh logs 7K3D --previous --tail 131072
```

## Recover a session by ID

Use the default action:

```bash
mesh recover 7K3D
```

If the saved session points to remote work, Mesh queries that exact host and
session. A surviving worker resumes without a new process. If the target is
interrupted, recovery runs on the target host. An unavailable target leaves the
saved record intact.

The SSH picker recovers sessions on the SSH host. For a saved target on another
host, it reports the exact host and session to connect to directly. Select
**Open shell** to stay on the SSH host. Mesh does not relay that terminal through
the SSH host. You can recover a local SSH-host session directly:

```bash
ssh -t pc.mesh.shaulavo.dev recover 7K3D
ssh -t pc.mesh.shaulavo.dev recover 7K3D --command
```

To open a shell on the original session's host, select `s` in the picker or run:

```bash
mesh recover 7K3D --shell
```

To explicitly restart its saved command, select `c` in the full picker or run:

```bash
mesh recover 7K3D --command
```

If the worker still runs, recovery uses it. An attached worker requires an
explicit takeover through the picker or `mesh recover 7K3D --takeover`.

## Save an explicit restart command

Save arguments as a command array:

```bash
mesh recovery-command 7K3D -- npm run dev -- --port 3000
```

This recipe runs only after **Restart command** or `--command`. It does not create
a restart loop. Without a custom recipe, the explicit command action uses the
original launch command.

A local recipe uses your current directory. A remote recipe uses the session's
saved directory. Pass `--cwd /absolute/path` to choose a directory on that host.

Clear the custom recipe:

```bash
mesh recovery-command 7K3D --clear
```

## Inspect previous attempts

Recovery creates a fresh session ID. The full picker groups older attempts below
their replacement and keeps their saved output. Retrying recovery of the source
finds the same replacement. Interrupted replacement chains resolve to the latest
attempt. An exited replacement stays finished until you explicitly select that
attempt for recovery.

If a launcher disappears before the worker answers, Mesh may report an uncertain
launch. It keeps the reserved ID and does not start a duplicate. Retry after the
worker publishes. After a host reboot, a changed boot identity permits a safe
retry of that reservation. On a host that cannot provide a boot identity, an
uncertain reservation requires manual inspection.

Exited and deliberately killed sessions stay in history. You can explicitly
recover them in the full picker. The compact window prompt excludes exited
sessions and previous attempts with an existing replacement.

Select a finished attempt and press `x` to forget it. Recovery does not delete the
source record automatically. `mesh --window --take` considers detached workers
only and starts a fresh shell when none can be claimed.

## Check the saved directory

Enable the [Bash or Zsh prompt hook](terminals.md#save-shell-directories-and-history)
to save the interactive shell's directory. The picker labels observed and launch
directory fallbacks when that hook is unavailable.

If the saved directory disappeared, shell recovery uses its nearest existing
parent and prints the changed path. Command recovery fails until its recorded
directory exists. Mesh keeps the source record in either case.

Checkpoints retain at most 256 rendered lines and 128 KiB of text. Changed output
saves periodically, so the checkpoint time can precede a crash. A failed save
leaves the previous checkpoint intact. An unsupported or corrupt checkpoint
remains visible as unavailable recovery details.

## Provider conversations

Shell recovery does not resume a Codex or Claude conversation automatically.
[Provider compatibility](agent-recovery-compatibility.md) records the offline
probe and remaining live checks for T25. Provider transcripts and authentication
remain with each provider on the host where it ran.

## Power-loss acceptance

Automated checks cover process termination, acknowledged checkpoints, concurrent
recovery, and simulated boot changes. They do not establish physical power-loss
durability. Run the [disposable VM acceptance procedure](recovery-powerloss.md)
before relying on stronger filesystem or shell-history guarantees.
