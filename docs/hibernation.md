# Hibernate idle agent sessions

A detached session keeps its process running, and a Codex or Claude process
holds hundreds of megabytes while it waits for you. Hibernation stops an idle
agent and keeps its conversation: attaching again reopens the exact
conversation in a fresh session, through the same native resume that
[conversation recovery](agent-recovery.md) uses.

Only an agent whose conversation Mesh has registered can hibernate. A plain
shell, or an agent started without the Mesh launch helper, has nothing Mesh can
reopen exactly, so Mesh leaves it running.

## Prerequisites

Hibernation resumes through agent recovery, so its setup applies:

```sh
mesh agent setup claude --install
mesh agent setup codex --install
```

Review and trust the Mesh hooks in Codex's `/hooks` screen. Then load the shell
wrappers so plain `claude` and `codex` commands register their conversations.
Put the line before any alias of the same name: the wrappers skip a command that
is already an alias, and an alias defined afterwards expands into the wrapper.

```sh
eval "$(mesh shell-init bash --agents)"
alias claude='claude --dangerously-skip-permissions'
```

`mesh agent doctor claude` reports whether the installed version is supported.
Mesh accepts the natively verified version and later releases in the same major
line: Claude Code 2.1.261 and Codex CLI 0.153.4 onward.

## Hibernate automatically

Start the host's daemon with an idle time:

```sh
mesh daemon --hibernate-idle 6h
```

A session qualifies once it has been detached for that long and its terminal
has printed nothing for that long. An agent that is still working prints
progress, so it keeps running until it finishes and then sits quiet for the
full period. The worker re-checks both clocks before it stops anything, so a
client attaching at the last moment wins.

Zero, the default, disables automatic hibernation. The daemon logs each
hibernated session and, once, why a candidate stayed running.

## Hibernate now, and reclaim memory

```sh
mesh hibernate 7K3D    # stop one detached agent now
mesh gc                # preview what idle sessions would be reclaimed
mesh gc --yes          # hibernate idle agents
mesh gc --shells --yes # also end idle plain shells
mesh gc --idle 1h      # change the idle threshold (default 6h)
```

`mesh hibernate` refuses an attached session and one with no registered agent,
and names the reason. `mesh gc` looks at this host and every known host. It
considers only detached sessions whose detach and last output are both at least
`--idle` ago, and never touches the terminal it runs in:

```
HOST       ID    MEM   IDLE  ACTION                      WHAT
this host  5Q3H  110M  7h    hibernate                   claude
this host  C8E7  57M   2d    left running (plain shell)  /bin/bash

reclaimable: 110M from 1 session; run mesh gc --yes to act
```

A plain shell ends only with `--shells`; its directory and history remain
available to [workspace recovery](recovery.md). Before ending one, `gc` checks
that it is still detached and has printed nothing since the plan.

`mesh ls` shows `hibernated` in the STATE column, how long each session has been
quiet in IDLE, and each live session's memory in MEM. On Linux MEM is the
proportional size of the worker and everything it runs, swapped pages included,
so shared pages are not counted twice. macOS reports resident size.

## Wake a session

Attach as usual: `mesh 7K3D`, or select it in the picker and press Enter.
`mesh pc -r` still prefers a live session; it wakes the most recently active
hibernated one only when nothing on that host is running. Mesh
reopens the saved conversation in a new session with the provider's own resume
command, in the recorded project directory, without sending a prompt. The old
session stays in history and points to its replacement. A second attach while
the first is starting reaches the same replacement.

A hibernated session's state is `exited` on the wire, with the hibernation
recorded alongside it, so older clients still list the host. Their picker
offers **Resume claude** or **Resume codex** for it, which wakes it the same way.

## What is kept and what is not

The provider keeps the conversation transcript, so the conversation resumes
exactly. The terminal's live process state does not survive: a command the
agent had running in the background ends, an unsent draft in the prompt box
may be lost, and scrollback beyond the saved checkpoint is gone. `mesh logs 7K3D --previous`
prints the retained output.

A hibernated session no longer holds the host awake. Detached live sessions
block idle sleep; hibernated ones are ended, so a quiet host can sleep.
