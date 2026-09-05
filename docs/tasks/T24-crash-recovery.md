# T24 — Recover your workspace after a crash

**Status:** implemented; automated checks passed; VM acceptance pending · **Prerequisites:** T04, T17, T22, T23

## Outcome

After a host crashes, open Mesh and recognize the work that was open. Select a
session to open a shell in its last saved directory, inspect previous output,
or reconnect to a remote session that survived. Recent shell commands remain
available through the shell's normal history.

[T25](T25-agent-recovery.md) adds exact Codex and Claude conversation recovery.
T24 must be useful without either tool installed.

The implementation follows this brief. The starting-point section records the
behavior before T24. Delivery notes below identify verification limits.

## Scope and user behavior

Keep the existing picker and compact window prompt. Do not add a recovery wizard.
Retain the title or project label, host, and last activity across a restart.
Show the checkpoint time and mark saved output as **Previous output**.

| Observed state | Default action | Result |
|---|---|---|
| Detached worker answers | Resume | Attach to the existing process. |
| Worker is already attached | Existing explicit takeover | Preserve T23's ownership rules. |
| Interrupted shell | Recover shell | Start the configured shell in the last saved shell directory. |
| Interrupted session with a saved remote target | Reconnect to target | Query that exact host and session after selection, then attach if available. |
| Interrupted arbitrary program | Open shell | Show its recorded command and offer a separate Restart command action. |
| Exited or deliberately killed session | Restart in history | Keep it out of automatic resume selection. Allow an explicit fresh start. |
| Offline host | Existing offline state | Keep recovery information on its owning host. Retry when reachable. |

Add `mesh recover SESSION` as the scriptable equivalent of the default recovery
action. `--shell` requests a shell. `--command` explicitly runs the saved restart
command. Reserve the command name in host-alias validation. Carry the selected
action in the protocol so local CLI, WebSocket clients, and SSH use the same rules.

Keep `mesh --window --take` detached-only. A terminal keybind must not restart
finished work or select a remote target without an explicit recovery selection.
List local recovery entries before any network request, as D29 requires.

## Starting point before T24

- [Worker metadata](../../internal/worker/meta.go) persists the launch command,
  launch directory, lifecycle state, and boot identity. It does not persist the
  last shell directory or a recovery recipe.
- [Inspection](../../internal/worker/inspection.go) already observes the foreground
  command, current directory, title, and bounded screen preview. Inspection is
  read-only. Its process directory may belong to an application rather than the shell.
- [Worker shutdown](../../internal/worker/worker.go) saves the replay ring at exit.
  A killed worker or power loss can skip that write.
- [CLI relaunch](../../internal/cli/window.go) and [SSH relaunch](../../internal/cli/ssh.go)
  separately create the original command in the launch directory, then forget
  the old session. Only `interrupted` is accepted.
- [Nesting](../../internal/worker/nesting.go) tracks exact host/session identities
  while connections remain open. A saved target must become a recovery hint,
  never resurrected live nesting state.
- [Catalog reconciliation](../../internal/daemon/catalog.go) already distinguishes
  interrupted and exited workers. Preserve this lifecycle model.

## Save a small recovery record on the owning host

Add `internal/recovery/` for versioned recovery records, bounded storage, recovery
selection, and the host-local restart transaction. Keep PTY ownership in the worker.
The daemon remains a coordinator and the sole SQLite writer.

Use a separate `recovery.json` beside `meta.json`. The worker owns this file while
alive. Shell helpers send bounded updates to the worker's Unix socket. They do
not edit the record or SQLite. A restart transaction uses separate files after
the source worker is proven unavailable, so it cannot race the live checkpoint writer.

Persist the following data:

| Field group | Meaning |
|---|---|
| Version, host ID, session ID, checkpoint time | Validate ownership and identify the last complete checkpoint. |
| Shell executable and last shell directory | Recover an interactive shell using its normal startup files. |
| Directory source | Distinguish shell-reported, observed, and launch-only locations. |
| Title, last output time, bounded text preview | Recognize the task after reboot. Treat titles as display text. |
| Launch command and optional explicit restart argv | Preserve arguments without reconstructing a shell command string. |
| Optional exact remote target | Host ID plus session ID from Mesh nesting registration. |

Add T25's optional provider record in T25, with its own versioned validation.
Keep lifecycle state in `meta.json`. Do not introduce a competing state machine
in the recovery record or duplicate all checkpoint data into SQLite.

Initial limits are implementation targets, subject to the IO check in milestone 1:

- Checkpoint changed data every two seconds while active. Skip unchanged writes.
- Keep at most 256 rendered output lines and 128 KiB of recovery text per session.
  Keep the visible preview within T22's existing wire limits.
- Write a temporary file, sync it, rename it, and sync the containing directory.
  Readers see a complete old or new record. Clean abandoned temporary files.
- Copy bounded state under the worker lock, then render and write outside the PTY
  pump and attachment locks. Use one pending snapshot, replacing stale queued data.
- On write failure, keep the previous checkpoint and expose its age. Do not stall
  input, detach, or output. Two seconds is a save cadence, not a loss guarantee.

Build saved text from rendered terminal state and bounded completed lines. Do not
inject stored ANSI into a fresh terminal or mix old output into new replay sequences.
Store files with the session directory's existing private permissions. Retain
recovery text until the user forgets the record. Display saved data only through
the owning host's existing authenticated routes, with its timestamp.

## Add a small Bash and Zsh integration

Generate an opt-in shell snippet through a proposed `mesh shell-init bash|zsh`.
Document one source line for each shell in `docs/terminals.md`. Running the snippet
twice must not duplicate hooks. It must be silent and harmless outside Mesh.

Use the proven immediate containing worker from `ContainingSessionWorker` to
register the shell. Treat `MESH_SESSION_ID` as a hint until ownership is verified.
Distinguish the registered interactive shell from subprocess shells opened by an
agent. An agent's `cd` must not replace the interactive shell's recovery directory.

On a prompt, report the shell directory and flush new history entries using the
shell's normal append mechanism. Preserve prompt exit status, existing prompt
hooks, history exclusions, and disabled-history settings. Do not install a global
Bash DEBUG trap or record terminal keystrokes. Test both scalar and array forms
of Bash `PROMPT_COMMAND` and Zsh's existing hook composition.

The first version promises saved completed commands after a prompt, not recovery
of an unsubmitted command line. File append improves crash recovery but is not
proof of power-loss durability. Validate the supported history files in the VM
acceptance check before claiming a stronger guarantee.

Without the integration, use the last defensible observed directory, then the
launch directory. Label that fallback. If a saved path is gone, retain the record
and offer a shell in the nearest existing parent with the changed path visible.
Do not silently run a saved program from a different directory.

## Restart once and retain the previous attempt

Expose one host-local recovery operation from `internal/recovery/`, called by
the direct local client and the daemon. The SSH adapter calls the daemon operation
instead of recreating its own create/remove sequence. Proposed control messages
are `session.recover` and `session.recovered`.

Use a fresh Mesh session ID for every replacement worker. Keep the old ID and
saved output as a previous attempt linked by `replacementID` and `recoveredFrom`.
The picker groups completed replacements under the current attempt and keeps the
project label. Do not reuse replay offsets or automatically delete old records.

Make recovery idempotent across retries and concurrent clients:

1. Acquire a host-local file lock for the source session. OS lock release must
   recover from a crashed caller. Recheck boot identity, metadata, and worker liveness.
2. Resolve an existing replacement before creating anything. Attach only through
   the existing detached-only claim or explicit takeover path.
3. Persist an intent with the reserved replacement ID, source ID, action, and
   launch parameters before spawning. Extend the existing launch reservation
   rather than relying on the daemon's in-memory request-ID cache.
4. Publish the new worker with the source identity in its durable metadata.
   Wait for its socket and creation acknowledgement before marking the link complete.
5. A retry reconciles the reserved ID and launch marker. It never guesses from a
   PID or starts a second worker while the first attempt may still be alive.

Represent an uncertain launch explicitly and retain it for reconciliation. A
failed directory check, spawn, or attach leaves the old recovery record intact.
For a plain shell, a published worker is sufficient to finish creation. T25 adds
a stronger acknowledgement before reporting a conversation as recovered.

For nested remote work, verify the saved exact target on its host after selection.
If it survived, attach there without recreating the old local shell chain. If the
target is interrupted, recover it on that host. If it is missing or offline, retain
the hint and offer retry or a local shell. Never choose the host's latest session
as a replacement. Existing containment and cycle checks still apply.

## Keep manual restart recipes small

Add `mesh recovery-command SESSION -- PROGRAM ARG...` and a `--clear` form.
Record argv and its working directory on the owning host. The full picker exposes
the saved command as Restart command. Execution requires that explicit action.

This covers a development server or watcher without a scheduler, restart loop,
shell-history replay, or per-program plugins. Programs that require shell syntax
can use an explicitly supplied `sh -lc` command. Never infer a restart recipe from
the foreground process name or terminal title.

## Implementation order and proof

Complete each milestone with its focused checks before widening scope:

1. **Checkpoint storage and background writer.** Add the bounded record and
   rendered text retention. Verify atomic reads, malformed and future-version
   records, disk-full failures, and a slow writer while PTY input/output continues.
   A killed worker leaves the last acknowledged checkpoint readable.
2. **Shell directory and history.** Add Bash and Zsh snippets and a bounded local
   registration message. Test changed directories, worktrees, multiple shells,
   prompt composition, exclusions, and daemon absence with real PTYs.
3. **Recovery transaction and transport parity.** Implement the shared operation,
   fresh IDs, retained attempts, and retry reconciliation. Migrate CLI, window,
   and SSH callers together. Race local and remote requests for the same source.
4. **Picker and remote continuation.** Add saved previews, checkpoint times,
   failure states, and exact remote-target reconnect. Keep local startup independent
   of the network. Test killed records and `--take` behavior.
5. **Explicit restart recipes and delivery.** Add argv configuration, compatibility
   behavior, user documentation, and the repeatable acceptance suite.

Add `scripts/check-t24.sh` when implementing. Follow `scripts/check-t23.sh` and
use isolated state directories. Retain these proposed integration scenarios:

- `crash_checkpoint.sh`: kill the worker after an acknowledged save, then recover
  the directory and previous output without letting it run its exit handler.
- `shell_recovery.sh`: complete commands, change directories, crash the session,
  and inspect history and the recovered shell in Bash and Zsh.
- `recovery_races.sh`: crash each restart boundary, drop replies, and issue
  simultaneous local, WebSocket, and SSH recovery requests. Produce one replacement.
- `recovery_remote_survival.sh`: lose the local worker and reconnect to the exact
  surviving remote session. Cover offline, attached, removed, and interrupted targets.
- `recovery_history.sh`: explicit restart of exited/killed sessions, retained
  previous output, missing directories, and user-initiated forgetting.

Keep the existing reboot simulation, window relaunch, inspection, nested detach,
and SSH integration checks green. Add a disposable-VM hard-power-off acceptance
test after a recorded checkpoint acknowledgement. Process termination and fake
boot IDs alone do not validate filesystem durability during power loss.

Run the repository checks from `CLAUDE.md` for implementation changes, including
all integration scripts and Linux/Darwin cross-builds. See the delivery notes below for completed checks and remaining operator acceptance.

## Compatibility, effort, and delivery boundary

Old sessions remain listable with launch-only recovery. Unknown recovery versions
show unavailable recovery details without hiding the session. Add capability
reporting for saved recovery and restart operations. Older workers keep live attach
behavior; unsupported clients can still list, attach, and use existing logs.
Keep legacy interrupted relaunch where the older peer explicitly supports it.
Do not show saved text as a live inspection or infer support from binary version alone.

Most display work reuses T22 and T23. The work requiring care is shell integration,
durable writes, and exactly-once replacement creation. `golang.org/x/sys` is already
a dependency and can provide host-local locking. No new runtime dependency is planned.

Defer desktop window placement, pane layouts, editor-specific recovery, environment
dumps, background-job reconstruction, full terminal recordings, cross-host filesystem
sync, and unattended command execution. T25 owns agent conversation semantics.

## Delivery notes

The runtime now includes atomic worker checkpoints, Bash and Zsh shell-init hooks,
a durable shared recovery transaction, local/WebSocket/SSH recovery adapters,
retained attempt groups, exact remote continuation from the Mesh CLI, explicit
command recipes, and `mesh logs SESSION --previous` for the full saved text.
The existing `.launching` reservation marker remains in use.

SSH remains local to its connected host. An exact remote hint produces a direct
connection instruction with its host/session identity and an Open shell option.
This preserves the existing rule against proxying sessions through another host.

Recovery stores dispatch boot identity. A provably different host boot permits a
retry of the same unpublished reservation. Same-boot uncertainty remains explicit;
a missing socket alone cannot prove that a launched worker never ran.

Run `scripts/check-t24.sh` for focused checks and `scripts/verify.sh` for the full
integration suite. Real Bash and Zsh PTYs exercise shell directory, history,
prompt composition, and nested-shell ownership. Set `MESH_TEST_ZSH` if Zsh is
outside PATH. A missing Zsh executable is an explicit skipped check.

The disposable VM hard-power-off procedure is in
[recovery-powerloss.md](../recovery-powerloss.md). It has not been run in this
development environment, which has no QEMU executable or disposable VM. No
physical power-loss or shell-history fsync guarantee is claimed.

The runtime concurrency checks cover CLI/Unix/WebSocket callers together and
SSH/Unix callers together. Transaction tests inject failures and lost replies at
reservation and dispatch boundaries; a real process-kill matrix at every such
boundary remains additional stress coverage. These checks are separate from the
unrun VM power-loss acceptance.

Verified 2026-09-05: `go test -race ./...`, `go vet ./...`,
`go mod tidy -diff`, formatting, all 34 integration scripts, and
`scripts/check-t24.sh` with Bash and Zsh 5.9.2. The focused checker also builds
CGO-disabled Linux amd64/arm64 and Darwin arm64 binaries. Full unlimited lint
reports the same eleven existing findings in bootstrap, installer, and tag code,
with none in the new recovery implementation.

SSH contention exposed a daemon response-ordering bug: a synchronous resize
error could overtake the worker's queued attachment rejection. All client
responses now use one bounded FIFO. A deterministic regression and ten
consecutive real SSH recovery runs verify the corrected order.
