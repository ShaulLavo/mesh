# CLI reference

Generated from `--help`. Do not edit by hand. Regenerate with:

```bash
python3 scripts/gen-cli-reference.py > docs/reference/cli.md
```

Run `mesh COMMAND --help` for the same text. A bare name is a host or a session, so
`mesh pc`, `mesh pc -r` and `mesh 7K3D` are not subcommands.

| Command | Job |
|---|---|
| [`mesh completion`](#mesh-completion) | Generate the autocompletion script for the specified shell |
| [`mesh agent`](#mesh-agent) | Run a native agent with conversation recovery |
| [`mesh attach`](#mesh-attach) | Attach to a session on this host |
| [`mesh back`](#mesh-back) | Return this terminal to the session it last opened |
| [`mesh context`](#mesh-context) | Open and manage durable working contexts for an agent |
| [`mesh gc`](#mesh-gc) | Reclaim memory from sessions left detached and idle |
| [`mesh hibernate`](#mesh-hibernate) | Stop an idle agent now; its conversation resumes on the next attach |
| [`mesh kill`](#mesh-kill) | End a session and wait until it is gone |
| [`mesh local`](#mesh-local) | Start or resume a session on this host |
| [`mesh logs`](#mesh-logs) | Print recent terminal output without attaching |
| [`mesh ls`](#mesh-ls) | List sessions across known hosts |
| [`mesh recover`](#mesh-recover) | Recover a saved session and retain its previous output |
| [`mesh recovery-command`](#mesh-recovery-command) | Save an explicit restart command |
| [`mesh rm`](#mesh-rm) | Forget finished sessions and delete what they left behind |
| [`mesh session`](#mesh-session) | Read sessions and run keyed detached commands |
| [`mesh sig`](#mesh-sig) | Send a signal to a session process group |
| [`mesh add`](#mesh-add) | Install Mesh on an SSH-reachable host |
| [`mesh cp`](#mesh-cp) | Copy files or directories through an authenticated Mesh connection |
| [`mesh dashboard`](#mesh-dashboard) | Watch machine usage, sessions, and services |
| [`mesh fleet`](#mesh-fleet) | Share connections with explicitly trusted devices |
| [`mesh hosts`](#mesh-hosts) | Read machine context without waking machines |
| [`mesh inspect`](#mesh-inspect) | Read full host and fleet notes and current access |
| [`mesh profile`](#mesh-profile) | Read and edit owner-written host and fleet notes |
| [`mesh rename`](#mesh-rename) | Change the destination machine's name |
| [`mesh ssh`](#mesh-ssh) | Open a shell or command that ends when the connection closes |
| [`mesh wake`](#mesh-wake) | Wake a configured host |
| [`mesh app`](#mesh-app) | Create and manage disposable websites |
| [`mesh serve`](#mesh-serve) | Publish or list services from adopted hosts |
| [`mesh unserve`](#mesh-unserve) | Remove one private service |
| [`mesh daemon`](#mesh-daemon) | Run this host's coordinator |
| [`mesh device`](#mesh-device) | Approve device keys for this daemon's OS account |
| [`mesh mcp`](#mesh-mcp) | Run the stateless native MCP server (stdio or loopback HTTP) |
| [`mesh private-names`](#mesh-private-names) | Manage private DNS and wildcard certificates |
| [`mesh shell-init`](#mesh-shell-init) | Print opt-in directory and history recovery hooks |
| [`mesh skill`](#mesh-skill) | Show and manage the embedded Mesh agent skill |
| [`mesh update`](#mesh-update) | Review and update Mesh on this machine or your saved fleet |
| [`mesh version`](#mesh-version) | Show the executing Mesh build |

## mesh

```text
Sessions live on the host that runs them, not on the connection.
Close your laptop, lose wifi, or quit mesh, and the command keeps
running. Reattach from anywhere on your tailnet.

A bare name is a host or a session, so the commands you use most are
not subcommands at all.

Agents: run mesh skill show.

USAGE

  mesh [command] [host|session] [-- command...] [--flags]

EXAMPLES

  # Pick a host and session
  mesh
  # Start a new session on pc
  mesh pc
  # Reattach to the newest session on pc
  mesh pc -r
  # Attach to a session by its id
  mesh 7K3D
  # Run one command there, then come back
  mesh pc -- htop
  # Detach and leave it running: ctrl+]

COMMANDS

  completion [command]                                  Generate the autocompletion script for the specified shell
  help [command]                                        Help about any command

SESSIONS

  agent PROVIDER -- ARG... [command]                    Run a native agent with conversation recovery
  attach session [--flags]                              Attach to a session on this host
  back [--flags]                                        Return this terminal to the session it last opened
  context [command]                                     Open and manage durable working contexts for an agent
  gc [--flags]                                          Reclaim memory from sessions left detached and idle
  hibernate session...                                  Stop an idle agent now; its conversation resumes on the next attach
  kill session                                          End a session and wait until it is gone
  local [-- command...] [--flags]                       Start or resume a session on this host
  logs session [--flags]                                Print recent terminal output without attaching
  ls [--flags]                                          List sessions across known hosts
  recover SESSION [--flags]                             Recover a saved session and retain its previous output
  recovery-command SESSION -- PROGRAM ARG... [--flags]  Save an explicit restart command
  rm session...                                         Forget finished sessions and delete what they left behind
  session [command] [--flags]                           Read sessions and run keyed detached commands
  sig session signal                                    Send a signal to a session process group

HOSTS

  add host [user@] [--flags]                            Install Mesh on an SSH-reachable host
  cp SOURCE DESTINATION [--flags]                       Copy files or directories through an authenticated Mesh connection
  dashboard [--flags]                                   Watch machine usage, sessions, and services
  fleet [command]                                       Share connections with explicitly trusted devices
  hosts [--flags]                                       Read machine context without waking machines
  inspect HOST [--flags]                                Read full host and fleet notes and current access
  profile [command] [--flags]                           Read and edit owner-written host and fleet notes
  rename host new-name                                  Change the destination machine's name
  ssh HOST [-- command...] [--flags]                    Open a shell or command that ends when the connection closes
  wake host [command]                                   Wake a configured host

SERVING

  app [command] [--flags]                               Create and manage disposable websites
  serve [command] [--flags]                             Publish or list services from adopted hosts
  unserve ROUTE [--flags]                               Remove one private service

SETUP

  daemon [command] [--flags]                            Run this host's coordinator
  device [command]                                      Approve device keys for this daemon's OS account
  mcp [command] [--flags]                               Run the stateless native MCP server (stdio or loopback HTTP)
  private-names [command]                               Manage private DNS and wildcard certificates
  shell-init bash|zsh [--flags]                         Print opt-in directory and history recovery hooks
  skill [command] [--flags]                             Show and manage the embedded Mesh agent skill
  update [command] [--flags]                            Review and update Mesh on this machine or your saved fleet
  version [--flags]                                     Show the executing Mesh build

FLAGS

  --detach-key                                          Key that detaches, such as ctrl+] or none
  -h --help                                             Help for mesh
  --host                                                Machine name or exact host ID; put its command after --
  --leave-key                                           Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                                             Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --raw                                                 Pass every input byte through without a detach key
  -r --resume                                           Resume the newest active session on the host, or wake a hibernated one when none is live
  --take                                                Resume the newest detached session without a prompt; requires --window
  -v --version                                          Version for mesh
  --window                                              Open a terminal window with a local persistent session
```

## mesh completion

```text
Generate the autocompletion script for mesh for the specified shell.
See each sub-command's help for details on how to use the generated script.


USAGE

  mesh completion [command] [--flags]

COMMANDS

  bash [--flags]        Generate the autocompletion script for bash
  fish [--flags]        Generate the autocompletion script for fish
  powershell [--flags]  Generate the autocompletion script for powershell
  zsh [--flags]         Generate the autocompletion script for zsh

FLAGS

  -h --help             Help for completion
  --leave-key           Key that leaves all nested sessions, or none (ctrl+^)
  --privacy             Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh agent

```text
Run a native agent with conversation recovery

Usage:
  mesh agent PROVIDER -- ARG... [flags]
  mesh agent [command]

Available Commands:
  bind        Verify and save an exact native conversation
  doctor      Check provider recovery hooks and compatibility
  setup       Print or install stable conversation recovery hooks

Flags:
  -h, --help   help for agent

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh agent [command] --help" for more information about a command.
```

## mesh agent bind

```text
Verify and save an exact native conversation

Usage:
  mesh agent bind PROVIDER CONVERSATION_ID [-- ARG...] [flags]

Flags:
  -h, --help   help for bind

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh agent doctor

```text
Check provider recovery hooks and compatibility

Usage:
  mesh agent doctor PROVIDER [flags]

Flags:
  -h, --help   help for doctor

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh agent setup

```text
Print or install stable conversation recovery hooks

Usage:
  mesh agent setup PROVIDER [flags]

Flags:
  -h, --help          help for setup
      --install       merge Mesh hooks into provider settings
      --path string   provider hook settings file
      --uninstall     remove Mesh hooks while preserving other settings

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh attach

```text
Attach to a session on this host

USAGE

  mesh attach session [--flags]

FLAGS

  --daemon      Attach through the local daemon
  --detach-key  Key that detaches, such as ctrl+] or none
  -h --help     Help for attach
  --leave-key   Key that leaves all nested sessions, or none (ctrl+^)
  --privacy     Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --raw         Pass every input byte through
```

## mesh back

```text
Return this terminal to the session it last opened.

Each terminal remembers the session it opened. If the host rebooted and the
session was interrupted, `back` recovers it the way `mesh recover` does and
rebinds this terminal to the replacement. Recovering a session that was running
an agent resumes that conversation; use --shell for a plain shell instead.

USAGE

  mesh back [--flags]

FLAGS

  --agent       Resume the exact saved agent conversation
  --command     Explicitly restart the saved command
  --detach-key  Key that detaches from the session (ctrl+])
  --forget      Drop this terminal's binding and return
  -h --help     Help for back
  --leave-key   Key that leaves all nested sessions, or none (ctrl+^)
  --privacy     Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --raw         Attach without changing terminal mode
  --shell       Open a shell in the saved directory
  --takeover    Take over if the session is already attached
```

## mesh context

```text
Open and manage durable working contexts for an agent

USAGE

  mesh context [command] [--flags]

COMMANDS

  close CONTEXT [--flags]   Stop new jobs in a context; running jobs keep going
  get CONTEXT [--flags]     Read one context
  ls [--flags]              List the application's contexts on a host
  open [--flags]            Open a durable working context on a host
  receipt [--flags]         Look up a context by the key that opened it
  update CONTEXT [--flags]  Change a context at an expected revision

FLAGS

  -h --help                 Help for context
  --leave-key               Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                 Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh context close

```text
Stop new jobs in a context; running jobs keep going

USAGE

  mesh context close CONTEXT [--flags]

FLAGS

  --expected-revision  Fail if the context changed since this revision
  -h --help            Help for close
  --host               Online machine name or exact host ID; implied by a context handle
  --identity           Local application identity, required
  --json               Write one agent result, required
  --leave-key          Key that leaves all nested sessions, or none (ctrl+^)
  --privacy            Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout            Hard deadline from 100ms to 60s (10s)
```

## mesh context get

```text
Read one context

USAGE

  mesh context get CONTEXT [--flags]

FLAGS

  -h --help    Help for get
  --host       Online machine name or exact host ID; implied by a context handle
  --identity   Local application identity, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline from 100ms to 60s (10s)
```

## mesh context ls

```text
List the application's contexts on a host

USAGE

  mesh context ls [--flags]

FLAGS

  -h --help    Help for ls
  --host       Online machine name or exact host ID; implied by a context handle
  --identity   Local application identity, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline from 100ms to 60s (10s)
```

## mesh context open

```text
Open a durable working context on a host

USAGE

  mesh context open [--flags]

FLAGS

  --cwd          Absolute directory on the target, required
  --env          Overlay variable NAME=VALUE, repeatable
  -h --help      Help for open
  --host         Online machine name or exact host ID; implied by a context handle
  --identity     Local application identity, required
  --json         Write one agent result, required
  --label        Display label
  --leave-key    Key that leaves all nested sessions, or none (ctrl+^)
  --privacy      Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --request-key  Saved logical request key, required
  --shell        Shell for command strings: login (default), sh or none
  --timeout      Hard deadline from 100ms to 60s (10s)
```

## mesh context receipt

```text
Look up a context by the key that opened it

USAGE

  mesh context receipt [--flags]

FLAGS

  -h --help      Help for receipt
  --host         Online machine name or exact host ID; implied by a context handle
  --identity     Local application identity, required
  --json         Write one agent result, required
  --leave-key    Key that leaves all nested sessions, or none (ctrl+^)
  --privacy      Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --request-key  Request key used to open, required
  --timeout      Hard deadline from 100ms to 60s (10s)
```

## mesh context update

```text
Change a context at an expected revision

USAGE

  mesh context update CONTEXT [--flags]

FLAGS

  --cwd                New absolute directory
  --expected-revision  Revision before this change, required
  -h --help            Help for update
  --host               Online machine name or exact host ID; implied by a context handle
  --identity           Local application identity, required
  --json               Write one agent result, required
  --label              New display label
  --leave-key          Key that leaves all nested sessions, or none (ctrl+^)
  --privacy            Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --set-env            Set overlay variable NAME=VALUE, repeatable
  --timeout            Hard deadline from 100ms to 60s (10s)
  --unset-env          Remove overlay variable NAME, repeatable
```

## mesh gc

```text
gc looks for sessions on this host and on known hosts that nobody is
attached to and that have printed nothing for --idle. Claude and Codex
sessions hibernate: their agent stops and resumes the same conversation
on the next attach. Plain shells are listed but left running unless
--shells is given, which ends them.

Without --yes, gc prints its plan and changes nothing. Attached sessions
and the session gc itself runs in are never touched.

USAGE

  mesh gc [--flags]

EXAMPLES

  # Show what would be reclaimed
  mesh gc
  # Hibernate agents detached and quiet for a day
  mesh gc --idle 24h --yes
  # Also end idle plain shells
  mesh gc --shells --yes

FLAGS

  -h --help    Help for gc
  --idle       Minimum time a session has been detached and silent (6h0m0s)
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --shells     Also end idle sessions that have no resumable agent
  --yes        Act on the plan instead of only printing it
```

## mesh hibernate

```text
Hibernate stops a detached Claude or Codex session to free its memory while
its conversation stays resumable. Attaching to the session later, with
mesh ID or from the picker, resumes the same conversation in a fresh
session. Plain shells cannot hibernate; use mesh kill for those.

USAGE

  mesh hibernate session... [--flags]

FLAGS

  -h --help    Help for hibernate
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh kill

```text
End a session and wait until it is gone

USAGE

  mesh kill session [--flags]

FLAGS

  -h --help    Help for kill
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh local

```text
Start or resume a session on this host

USAGE

  mesh local [-- command...] [--flags]

FLAGS

  --daemon      Require the local daemon
  --detach-key  Key that detaches, such as ctrl+] or none
  -h --help     Help for local
  --leave-key   Key that leaves all nested sessions, or none (ctrl+^)
  --privacy     Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --raw         Pass every input byte through
  -r --resume   Resume the latest live session
```

## mesh logs

```text
Print recent terminal output without attaching

USAGE

  mesh logs session [--flags]

FLAGS

  -h --help    Help for logs
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --previous   Print the saved rendered checkpoint with its timestamp
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --tail       Maximum trailing bytes to print (65536)
```

## mesh ls

```text
List sessions across known hosts

USAGE

  mesh ls [--flags]

FLAGS

  -a --all     Include exited and interrupted sessions
  --daemon     Read only the local daemon catalog
  -h --help    Help for ls
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Maximum wait for each host catalog reply after authenticated setup (local daemon default 500ms) (750ms)
```

## mesh recover

```text
Recover a saved session and retain its previous output

USAGE

  mesh recover SESSION [--flags]

FLAGS

  --agent       Resume the exact saved agent conversation
  --command     Explicitly restart the saved command
  --detach-key  Key that detaches from the session (ctrl+])
  -h --help     Help for recover
  --leave-key   Key that leaves all nested sessions, or none (ctrl+^)
  --privacy     Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --raw         Attach without changing terminal mode
  --shell       Open a shell in the saved directory
  --takeover    Take over if the recovered session is already attached
```

## mesh recovery-command

```text
Save an explicit restart command

USAGE

  mesh recovery-command SESSION -- PROGRAM ARG... [--flags]

FLAGS

  --clear      Clear the explicit restart command
  --cwd        Absolute directory on the session host for the command
  -h --help    Help for recovery-command
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh rm

```text
Forget finished sessions and delete what they left behind

USAGE

  mesh rm session... [--flags]

FLAGS

  -h --help    Help for rm
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh session

```text
Read sessions and run keyed detached commands

USAGE

  mesh session [command] [--flags]

COMMANDS

  create -- command [args...] [--flags]  Start a keyed detached command on an online host
  input SESSION_ID [--flags]             Write pipe stdin once or look up a write receipt
  inspect SESSION_ID [--flags]           Read live or saved session details without attaching
  kill SESSION_ID [--flags]              Stop the exact keyed command instance
  list [--flags]                         List one host's sessions with bounded pages
  logs SESSION_ID [--flags]              Read exact bounded output bytes with an independent cursor
  request [--flags]                      Look up a retained creation receipt without launching work
  rm SESSION_ID [--flags]                Remove a finished keyed command payload while retaining its receipt
  signal SESSION_ID [--flags]            Signal the exact keyed command instance
  wait SESSION_ID [--flags]              Wait for a durable result without stopping the command

FLAGS

  -h --help                              Help for session
  --json                                 Write one agent result
  --leave-key                            Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                              Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh session create

```text
Start a keyed detached command on an online host

USAGE

  mesh session create -- command [args...] [--flags]

FLAGS

  --cols           Initial terminal width (80)
  --context        With --identity, the context ID or handle of the job
  --cwd            Absolute clean working directory on the target
  -h --help        Help for create
  --host           Machine name or exact host ID, required
  --identity       Act as this local application identity
  --json           Write one agent result, required
  --kind           Explicit session kind, pty or pipe
  --leave-key      Key that leaves all nested sessions, or none (ctrl+^)
  --privacy        Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --request-key    Saved logical request key, required
  --rows           Initial terminal height (24)
  --shell-command  With --identity, a command string run through the context's shell
  --stdin          Pipe stdin mode, closed by default or open for keyed writes
  --term           Target terminal type (dumb)
  --timeout        Hard deadline from 100ms to 60s (40s)
```

## mesh session input

```text
Write pipe stdin once or look up a write receipt

USAGE

  mesh session input SESSION_ID [--flags]

FLAGS

  --base64     Write Base64-decoded bytes
  --context    With --identity, the context ID or handle of the job
  --eof        Close pipe stdin after these bytes
  --file       Write up to 1 MiB from a local file, or - for stdin
  -h --help    Help for input
  --host       Online machine name or exact host ID, required
  --identity   Act as this local application identity
  --instance   Exact instance from the creation receipt, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --offset     Expected byte offset, omitted to look up the write key
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --text       Write exact text bytes
  --timeout    Finite deadline from 100ms to 60s (5s)
  --write-key  Saved key for one logical write, required
```

## mesh session inspect

```text
Read a session's launch metadata and live or saved inspection.
Without --preview, Mesh reads one cell internally and discards its preview.
With --preview, Mesh reads at most 80 columns by 24 rows. Neither changes
terminal size or attachment.

USAGE

  mesh session inspect SESSION_ID [--flags]

FLAGS

  -h --help    Help for inspect
  --host       Machine name or exact host ID, required
  --instance   Read the exact keyed command receipt
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --preview    Include a control-free preview, at most 80 by 24
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for the read, from 100ms to 60s (5s)
```

## mesh session kill

```text
Stop the exact keyed command instance

USAGE

  mesh session kill SESSION_ID [--flags]

FLAGS

  --context    With --identity, the context ID or handle of the job
  -h --help    Help for kill
  --host       Online machine name or exact host ID, required
  --identity   Act as this local application identity
  --instance   Exact instance from the creation receipt, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Finite deadline from 100ms to 60s (5s)
```

## mesh session list

```text
List one host's sessions with bounded pages

USAGE

  mesh session list [--flags]

FLAGS

  --all        Include exited and interrupted sessions
  --detail     Include launch command and working directory
  -h --help    Help for list
  --host       Machine name or exact host ID, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --limit      Maximum sessions in this page, from 1 to 500 (100)
  --page       Opaque nextPage token from this host
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for the read, from 100ms to 60s (5s)
```

## mesh session logs

```text
Read exact bounded output bytes with an independent cursor

USAGE

  mesh session logs SESSION_ID [--flags]

FLAGS

  --context    With --identity, the context ID or handle of the job
  --cursor     Saved independent output cursor
  -h --help    Help for logs
  --host       Online machine name or exact host ID, required
  --identity   Act as this local application identity
  --instance   Exact instance from the creation receipt, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --max-bytes  Decoded page size from 1 to 1048576 bytes (65536)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --stream     Exact stream, pty, stdout or stderr (pty)
  --timeout    Finite deadline from 100ms to 60s (5s)
```

## mesh session request

```text
Look up a retained creation receipt without launching work

USAGE

  mesh session request [--flags]

FLAGS

  --context      With --identity, the context ID or handle of the job
  -h --help      Help for request
  --host         Machine name or exact host ID, required
  --identity     Act as this local application identity
  --json         Write one agent result, required
  --leave-key    Key that leaves all nested sessions, or none (ctrl+^)
  --privacy      Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --request-key  Saved logical request key, required
  --timeout      Hard deadline from 100ms to 60s (5s)
```

## mesh session rm

```text
Remove a finished keyed command payload while retaining its receipt

USAGE

  mesh session rm SESSION_ID [--flags]

FLAGS

  --context    With --identity, the context ID or handle of the job
  -h --help    Help for rm
  --host       Online machine name or exact host ID, required
  --identity   Act as this local application identity
  --instance   Exact instance from the creation receipt, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Finite deadline from 100ms to 60s (5s)
```

## mesh session signal

```text
Signal the exact keyed command instance

USAGE

  mesh session signal SESSION_ID [--flags]

FLAGS

  --context    With --identity, the context ID or handle of the job
  -h --help    Help for signal
  --host       Online machine name or exact host ID, required
  --identity   Act as this local application identity
  --instance   Exact instance from the creation receipt, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --signal     Process group signal, required
  --timeout    Finite deadline from 100ms to 60s (5s)
```

## mesh session wait

```text
Wait for a durable result without stopping the command

USAGE

  mesh session wait SESSION_ID [--flags]

FLAGS

  --context    With --identity, the context ID or handle of the job
  -h --help    Help for wait
  --host       Online machine name or exact host ID, required
  --identity   Act as this local application identity
  --instance   Exact instance from the creation receipt, required
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Finite deadline from 100ms to 60s (5s)
```

## mesh sig

```text
Send a signal to a session process group

USAGE

  mesh sig session signal [--flags]

FLAGS

  -h --help    Help for sig
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh add

```text
Install Mesh on an SSH-reachable host

USAGE

  mesh add host [user@] [--flags]

FLAGS

  --allow-root               Acknowledge that this device and trusted sharing devices receive root access
  -h --help                  Help for add
  --identity-file            Ssh private key to adopt with, when ~/.ssh/config names none
  --leave-key                Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                  Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --tailscale-auth-key-file  Read a Tailscale auth key from this local file
  --yes                      Approve remote Tailscale installation and user lingering changes
```

## mesh cp

```text
Copy between this machine and one enrolled Mesh host.
Use HOST:/absolute/path for the remote path and -r for directories.
An existing destination directory receives the source basename. Files are
replaced atomically, directories merge, and destination-only files stay.
Dotfiles, empty directories, and permission bits are preserved. Symbolic
links and special files are refused. No saved terminal session is created.

USAGE

  mesh cp SOURCE DESTINATION [--flags]

EXAMPLES

  mesh cp ./artifact.wasm upcloud-4:/srv/agent-work/artifact.wasm
  mesh cp -r ./source upcloud-2:/srv/agent-work/checkouts/
  mesh cp -r upcloud-4:/srv/agent-work/results ./results

FLAGS

  -h --help       Help for cp
  --leave-key     Key that leaves all nested sessions, or none (ctrl+^)
  --privacy       Mask personal data for recording and screen sharing (MESH_PRIVACY)
  -r --recursive  Copy a directory and its contents
```

## mesh dashboard

```text
Watch machine usage, sessions, and services

USAGE

  mesh dashboard [--flags]

FLAGS

  -h --help    Help for dashboard
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --theme      Dashboard theme (current, rose-pine, rose-pine-moon, oled, kanagawa, gruvbox-material); overrides config
  --wall       Show the passive fullscreen fleet view
```

## mesh fleet

```text
Share connections with explicitly trusted devices

USAGE

  mesh fleet [command] [--flags]

COMMANDS

  sync                    Fetch connections from trusted devices
  trust HOST [--flags]    Allow this device to share connections and access newly added servers
  untrust HOST [--flags]  Stop sharing connections with this device

FLAGS

  -h --help               Help for fleet
  --leave-key             Key that leaves all nested sessions, or none (ctrl+^)
  --privacy               Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh fleet sync

```text
Fetch connections from trusted devices

USAGE

  mesh fleet sync [--flags]

FLAGS

  -h --help    Help for sync
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh fleet trust

```text
Allow this device to share connections and access newly added servers

USAGE

  mesh fleet trust HOST [--flags]

FLAGS

  -h --help    Help for trust
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --yes        Approve this device's connection-sharing policy
```

## mesh fleet untrust

```text
Stop sharing connections with this device

USAGE

  mesh fleet untrust HOST [--flags]

FLAGS

  -h --help    Help for untrust
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --yes        Approve this device's connection-sharing policy
```

## mesh hosts

```text
Read machine context without waking machines

USAGE

  mesh hosts [--flags]

FLAGS

  --cached     Read local files and cached observations without dialing
  -h --help    Help for hosts
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for host queries (100ms to 30s) (2s)
```

## mesh inspect

```text
Read full host and fleet notes and current access

USAGE

  mesh inspect HOST [--flags]

FLAGS

  --cached     Read local files and cached observations without dialing
  -h --help    Help for inspect
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for host queries (100ms to 60s) (5s)
```

## mesh profile

```text
Read and edit owner-written host and fleet notes

USAGE

  mesh profile [command] [--flags]

COMMANDS

  edit [--flags]    Edit the local profile with VISUAL or EDITOR, for humans only
  set [--flags]     Replace host or fleet notes at an expected revision
  show [--flags]    Read the local host or fleet profile without contacting peers
  status [--flags]  Read local fleet delivery state without contacting members

FLAGS

  -h --help         Help for profile
  --json            Write one agent result
  --leave-key       Key that leaves all nested sessions, or none (ctrl+^)
  --privacy         Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh profile edit

```text
Edit the local profile with VISUAL or EDITOR, for humans only

USAGE

  mesh profile edit [--flags]

FLAGS

  --fleet      Edit signed fleet notes
  -h --help    Help for edit
  --json       Unsupported; use profile set for JSON input
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh profile set

```text
Replace host or fleet notes at an expected revision

USAGE

  mesh profile set [--flags]

FLAGS

  --expected-revision  Revision read before this edit; 0 for an absent profile
  --file               Editable JSON path, or - to explicitly read stdin
  --fleet              Replace signed fleet notes and queue saved members
  -h --help            Help for set
  --json               Write one agent result
  --leave-key          Key that leaves all nested sessions, or none (ctrl+^)
  --privacy            Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh profile show

```text
Read the local host or fleet profile without contacting peers

USAGE

  mesh profile show [--flags]

FLAGS

  --fleet      Read the local signed fleet replica
  -h --help    Help for show
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh profile status

```text
Read local fleet delivery state without contacting members

USAGE

  mesh profile status [--flags]

FLAGS

  --fleet      Read fleet delivery state
  -h --help    Help for status
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --retry      Reset pending and stale delivery age and retry
```

## mesh rename

```text
Change the destination machine's name

USAGE

  mesh rename host new-name [--flags]

FLAGS

  -h --help    Help for rename
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh ssh

```text
Run directly through the host's private Mesh SSH listener.
This connection has no saved session, detach key, or reconnection.

USAGE

  mesh ssh HOST [-- command...] [--flags]

FLAGS

  --cwd        Remote working directory; relative paths start at the account home
  --env        Remote KEY=VALUE override; repeatable, last value wins
  -h --help    Help for ssh
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  -T --no-pty  Use separate byte-preserving stdout and stderr without a terminal
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH listener port (2222)
```

## mesh wake

```text
Wake a configured host

USAGE

  mesh wake host [command] [--flags]

COMMANDS

  allow        Allow Mesh machines to wake this host
  deny         Stop allowing Mesh machines to wake this host

FLAGS

  -h --help    Help for wake
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh wake allow

```text
Allow Mesh machines to wake this host

USAGE

  mesh wake allow [--flags]

FLAGS

  -h --help    Help for allow
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh wake deny

```text
Stop allowing Mesh machines to wake this host

USAGE

  mesh wake deny [--flags]

FLAGS

  -h --help    Help for deny
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh app

```text
Create and manage disposable websites

USAGE

  mesh app [command] [--flags]

EXAMPLES

  mesh app create pc ./site
  mesh app create local ./backend --run 'bun run start' --port 3000
  mesh app browser approve pc CODE

COMMANDS

  browser [command] [--flags]         Approve and revoke owner browsers
  create HOST SOURCE... [--flags]     Create a temporary app from files or a directory
  delete HOST ID                      Delete a temporary app
  download HOST ID DEST               Save an owner's app source archive
  inspect HOST ID                     Inspect a temporary app
  list HOST                           List apps owned by a host
  renew HOST ID                       Renew a temporary app
  update HOST ID SOURCE... [--flags]  Update a temporary app from files or a directory

FLAGS

  -h --help                           Help for app
  --json                              Print machine-readable results
  --leave-key                         Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                           Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port                          Mesh SSH port for remote app hosts (2222)
```

## mesh app browser

```text
Approve and revoke owner browsers

USAGE

  mesh app browser [command] [--flags]

COMMANDS

  approve HOST CODE [--flags]  Approve owner browser grants
  list HOST                    List owner browser grants
  revoke HOST ID               Revoke owner browser grants

FLAGS

  -h --help                    Help for browser
  --json                       Print machine-readable results
  --leave-key                  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port                   Mesh SSH port for remote app hosts (2222)
```

## mesh app browser approve

```text
approve owner browser grants

USAGE

  mesh app browser approve HOST CODE [--flags]

FLAGS

  -h --help    Help for approve
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH port for remote app hosts (2222)
  --yes        Skip the human browser confirmation for scripts
```

## mesh app browser list

```text
list owner browser grants

USAGE

  mesh app browser list HOST [--flags]

FLAGS

  -h --help    Help for list
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh app browser revoke

```text
revoke owner browser grants

USAGE

  mesh app browser revoke HOST ID [--flags]

FLAGS

  -h --help    Help for revoke
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh app create

```text
create a temporary app from files or a directory

USAGE

  mesh app create HOST SOURCE... [--flags]

FLAGS

  --env        Server environment variable NAME=VALUE; repeatable
  -h --help    Help for create
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --max-size   Maximum compressed and expanded source size, e.g. 1GiB; 0 or unlimited removes the byte limit (64MiB)
  --port       Http server port; required with --run
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --run        Explicit HTTP server command; omit for static files
  --setup      Explicit setup command in the managed workspace
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh app delete

```text
delete a temporary app

USAGE

  mesh app delete HOST ID [--flags]

FLAGS

  -h --help    Help for delete
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh app download

```text
Save an owner's app source archive

USAGE

  mesh app download HOST ID DEST [--flags]

FLAGS

  -h --help    Help for download
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh app inspect

```text
inspect a temporary app

USAGE

  mesh app inspect HOST ID [--flags]

FLAGS

  -h --help    Help for inspect
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh app list

```text
List apps owned by a host

USAGE

  mesh app list HOST [--flags]

FLAGS

  -h --help    Help for list
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh app renew

```text
renew a temporary app

USAGE

  mesh app renew HOST ID [--flags]

FLAGS

  -h --help    Help for renew
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh app update

```text
update a temporary app from files or a directory

USAGE

  mesh app update HOST ID SOURCE... [--flags]

FLAGS

  --env        Server environment variable NAME=VALUE; repeatable
  -h --help    Help for update
  --json       Print machine-readable results
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --max-size   Maximum compressed and expanded source size, e.g. 1GiB; 0 or unlimited removes the byte limit (64MiB)
  --port       Http server port; required with --run
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --run        Explicit HTTP server command; omit for static files
  --setup      Explicit setup command in the managed workspace
  --ssh-port   Mesh SSH port for remote app hosts (2222)
```

## mesh serve

```text
Publish or list services from adopted hosts

USAGE

  mesh serve [command] [--flags]

EXAMPLES

  mesh serve HOST TARGET --at ROUTE
  mesh serve pc 5173 --at /dev --run 'bun run dev' --listen 5173=15173
  mesh serve pc --run 'bun run dev' --listen 5173=15173 --listen 3001=13001
  mesh serve pc 9000 --at /models --run ./run --cwd /srv/models --keep-running
  mesh serve ls
  mesh serve list
  mesh serve start ROUTE
  mesh serve stop ROUTE

COMMANDS

  label ROUTE NAME [--flags]  Set the display name of an existing service
  ls [--flags]                List live and cached services
  start ROUTE [--flags]       Start an on-demand route now and wait until it is ready
  stop ROUTE [--flags]        Stop an on-demand route now

FLAGS

  --at                        Route path, such as /blog
  --cwd                       Directory --run starts in (default: this directory)
  --env                       Key=Value added to the environment of --run (repeatable)
  --files                     Enable directory listings
  -h --help                   Help for serve
  --idle                      Stop --run after this long with no connection through a Mesh listener or tailnet route; only those connections count (15m0s)
  --isolate                   Send cross-origin isolation headers so the page can use SharedArrayBuffer
  --keep-running              Start --run now, never stop it for idleness, and restart it after it exits; for clients that use the upstream port directly
  --label                     Display name shown in service lists
  --leave-key                 Key that leaves all nested sessions, or none (ctrl+^)
  --listen                    Public=Upstream: proxy 127.0.0.1:PUBLIC on the host to 127.0.0.1:UPSTREAM (repeatable)
  --privacy                   Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --private-host              Private hostname at the root, using a label or a configured deployment hostname
  --ready-timeout             How long a starting --run holds connections before failing them (1m0s)
  --run                       Command that serves the port; started on the first connection through Mesh and stopped when idle, or kept running with --keep-running
```

## mesh serve label

```text
Set the display name of an existing service

USAGE

  mesh serve label ROUTE NAME [--flags]

FLAGS

  -h --help    Help for label
  --host       Machine name or exact host ID when more than one host owns ROUTE
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh serve ls

```text
List live and cached services

USAGE

  mesh serve ls [--flags]

FLAGS

  -h --help    Help for ls
  --json       Write one agent result with live and cached services
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for live host queries (2s)
```

## mesh serve start

```text
Start an on-demand route now and wait until it is ready. This ends the stop of a --keep-running
route, and starts it without waiting for its restart delay.

USAGE

  mesh serve start ROUTE [--flags]

FLAGS

  -h --help    Help for start
  --host       Machine name or exact host ID when more than one host owns ROUTE
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for ownership discovery (2s)
```

## mesh serve stop

```text
Stop an on-demand route now. Its listeners stay bound. A route with an idle window starts again on its
next connection through Mesh. A --keep-running route stays stopped until mesh serve start, a change
of its definition, or a daemon restart.

USAGE

  mesh serve stop ROUTE [--flags]

FLAGS

  -h --help    Help for stop
  --host       Machine name or exact host ID when more than one host owns ROUTE
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for ownership discovery (2s)
```

## mesh unserve

```text
Remove one private service

USAGE

  mesh unserve ROUTE [--flags]

FLAGS

  -h --help    Help for unserve
  --host       Machine name or exact host ID when more than one host owns ROUTE
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for ownership discovery (2s)
```

## mesh daemon

```text
Run this host's coordinator

USAGE

  mesh daemon [command] [--flags]

COMMANDS

  install [--flags]                 Install and start this machine's Mesh daemon

FLAGS

  --app-data-root                   Managed temporary app workload root; Linux defaults to /work/mesh/apps
  --app-registry-config             Private HTTPS app registry and origin allowlist config file
  --app-registry-target             Pinned private app registry config file
  --certificate-renewer-id          Pinned Mesh identity allowed to install certificates
  -h --help                         Help for daemon
  --hibernate-idle                  Stop a Codex or Claude conversation after its session is detached and quiet this long, resuming it on attach; zero disables (0s)
  --https-port                      Loopback HTTPS service port; zero disables HTTPS
  --leave-key                       Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                         Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --private-names-config            Pi private-name reconciliation config file
  --ssh-port                        Tailnet SSH port; zero disables SSH
  --subscriber-limit                Maximum state.watch subscribers across all transports (64)
  --tailnet-connection-limit        Maximum concurrent Tailnet control connections across all addresses (128)
  --tailnet-port                    Tailnet WebSocket port; zero disables remote listening
  --tailscale-serve                 Persist raw Tailscale TCP/443 forwarding to the HTTPS port
  --tailscale-serve-port            Loopback TLS gateway port; zero forwards directly to --https-port
  --tailscale-serve-proxy-protocol  Require verified PROXY v1 client addresses for private HTTPS and Tailscale Serve
  --unix-connection-limit           Maximum concurrent Unix control connections (128)
  --websocket-path                  Tailnet WebSocket path (/mesh)
```

## mesh daemon install

```text
Install the service that runs this machine's daemon, so it can host
sessions and be adopted by another host. Installing Mesh from a package
manager gives you the command but no daemon.

USAGE

  mesh daemon install [--flags]

FLAGS

  -h --help    Help for install
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --yes        Install without asking
```

## mesh device

```text
Approve device keys for this daemon's OS account

USAGE

  mesh device [command] [--flags]

COMMANDS

  approve ID [--flags]           Grant a device this daemon account's control and SSH access
  approve-checked ID [--flags]   Check the local daemon account, state and pinned identity before approval
  approve-fleet HOST… [--flags]  Approve this device on explicitly selected pinned hosts using system SSH
  identity [--flags]             Show this device's Mesh key and daemon account
  revoke ID                      Remove a device's session, service and SSH grant
  scope [command] [--flags]      Manage dedicated scoped TLS identities

FLAGS

  -h --help                      Help for device
  --leave-key                    Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                      Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh device approve

```text
Grant a device this daemon account's control and SSH access

USAGE

  mesh device approve ID [--flags]

FLAGS

  --allow-root  Acknowledge that approval grants root access
  -h --help     Help for approve
  --json        Write whether device approval changed as JSON
  --leave-key   Key that leaves all nested sessions, or none (ctrl+^)
  --privacy     Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --public-key  Read ID as an unrestricted OpenSSH Ed25519 public key
```

## mesh device approve-checked

```text
Check the local daemon account, state and pinned identity before approval

USAGE

  mesh device approve-checked ID [--flags]

FLAGS

  --account      Expected local daemon OS account
  --allow-root   Acknowledge that approval grants root access
  --check        Check existing state without writing a grant
  --destination  Expected destination Mesh identity pin
  -h --help      Help for approve-checked
  --leave-key    Key that leaves all nested sessions, or none (ctrl+^)
  --privacy      Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --state-dir    Existing canonical daemon state directory
  --yes          Grant the source full control and SSH access
```

## mesh device approve-fleet

```text
Approve this device on explicitly selected pinned hosts using system SSH

USAGE

  mesh device approve-fleet HOST… [--flags]

FLAGS

  --admin-map   Json map of selected host labels to explicit SSH target, account, stateDir and staged binary
  --allow-root  Acknowledge selected destinations that grant root access
  --check       Preview each selected destination without writing grants
  -h --help     Help for approve-fleet
  --leave-key   Key that leaves all nested sessions, or none (ctrl+^)
  --privacy     Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --yes         Grant this source full control and SSH access on the selected destinations
```

## mesh device identity

```text
Show this device's Mesh key and daemon account

USAGE

  mesh device identity [--flags]

FLAGS

  -h --help    Help for identity
  --json       Write identity, account and fingerprint as JSON
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh device revoke

```text
Remove a device's session, service and SSH grant

USAGE

  mesh device revoke ID [--flags]

FLAGS

  -h --help    Help for revoke
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh device scope

```text
Manage dedicated scoped TLS identities

USAGE

  mesh device scope [command] [--flags]

COMMANDS

  grant [--flags]   Grant host-wide read permissions locally at an expected revision
  keygen [--flags]  Create or load a key in an explicit private directory
  list [--flags]    Read this target's scope policy without contacting a daemon
  output [--flags]  Read a bounded tail or preview using an explicit scoped TLS key
  read [--flags]    Read one metadata page using only an explicit key and direct TLS
  revoke [--flags]  Remove a scoped identity locally at an expected revision

FLAGS

  -h --help         Help for scope
  --json            Write one agent result
  --leave-key       Key that leaves all nested sessions, or none (ctrl+^)
  --privacy         Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh device scope grant

```text
Grant host-wide read permissions locally at an expected revision

USAGE

  mesh device scope grant [--flags]

FLAGS

  --expected-revision  Current local scope policy revision
  --expires-at         Absolute RFC3339 expiry on this target
  --expires-in         Target-Clock lifetime, at most seven days (24h0m0s)
  -h --help            Help for grant
  --identity           Dedicated public identity
  --json               Write one agent result
  --leave-key          Key that leaves all nested sessions, or none (ctrl+^)
  --permissions        Metadata or metadata,session-output; output covers every host session and diagnostic-mixed tails
  --privacy            Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh device scope keygen

```text
Create or load a key in an explicit private directory

USAGE

  mesh device scope keygen [--flags]

FLAGS

  -h --help    Help for keygen
  --json       Write one agent result
  --key-dir    Dedicated credential directory; never the normal Mesh key
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh device scope list

```text
Read this target's scope policy without contacting a daemon

USAGE

  mesh device scope list [--flags]

FLAGS

  -h --help    Help for list
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh device scope output

```text
Read a bounded tail or preview using an explicit scoped TLS key

USAGE

  mesh device scope output [--flags]

FLAGS

  --cols       Preview display columns from 1 to 160; default 80
  --format     Tail or preview; requires session-output permission
  -h --help    Help for output
  --host       Exact saved host ID with an existing destination pin
  --json       Write one agent result
  --key-dir    Explicit dedicated credential directory
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --max-bytes  Tail byte limit from 1 to 1048576; default 65536
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --rows       Preview rows from 1 to 24; default 24
  --session    Exact session ID on the target
  --timeout    Hard deadline from 100ms to 60s (5s)
```

## mesh device scope read

```text
Read one metadata page using only an explicit key and direct TLS

USAGE

  mesh device scope read [--flags]

FLAGS

  --cursor     Continuation from this credential and section
  -h --help    Help for read
  --host       Exact saved host ID with an existing destination pin
  --json       Write one agent result
  --key-dir    Explicit dedicated credential directory
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --limit      Page size from 1 to 500; default 100
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --section    Host, sessions or services (host)
  --session    Exact session ID for the sessions section
  --timeout    Hard deadline from 100ms to 60s (5s)
```

## mesh device scope revoke

```text
Remove a scoped identity locally at an expected revision

USAGE

  mesh device scope revoke [--flags]

FLAGS

  --expected-revision  Current local scope policy revision
  -h --help            Help for revoke
  --identity           Scoped public identity to remove
  --json               Write one agent result
  --leave-key          Key that leaves all nested sessions, or none (ctrl+^)
  --privacy            Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp

```text
Run Mesh's stateless MCP 2026-07-28 server over stdio, or over loopback
Streamable HTTP when --http is passed.

In stdio mode, stdout carries only newline-delimited JSON-RPC MCP messages;
diagnostics and startup errors go to stderr. Pass --identity NAME to enable
durable execution contexts and mesh_exec; without --identity, only passive
native discovery is enabled.

In HTTP mode (--http 127.0.0.1:PORT --identity NAME), Mesh binds only the
specified loopback address at /mcp and authenticates every POST with a
random bearer token stored in a private 0600 file. Rotate tokens with
`mesh mcp token rotate`.

Usage:
  mesh mcp [flags]
  mesh mcp [command]

Available Commands:
  connect     Connect over stdio to one hosted MCP server on any Mesh host
  identity    Manage per-agent application identities
  logs        Print a hosted MCP server instance's recent stderr
  ls          List hosted MCP servers across saved Mesh hosts
  profile     Manage aggregate gateway profiles in mcp-profiles.json
  publish     Publish an MCP server hosted on a Mesh machine
  restart     Replace a hosted MCP server instance's process
  rm          Remove a hosted MCP server and stop its processes
  show        Show one hosted MCP server and its instances
  start       Start a hosted MCP server's instance now
  stop        Stop a hosted MCP server's processes and keep its definition
  token       Manage bearer tokens for the native loopback MCP HTTP server

Flags:
      --allow-origin stringArray   allow an exact loopback HTTP Origin header (repeatable; requires --http)
  -h, --help                       help for mcp
      --http string                bind the native Streamable HTTP server on one loopback host:port (for example 127.0.0.1:8765)
      --identity string            local application identity created with mesh mcp identity create NAME
      --profile string             serve the aggregate gateway for one profile from mcp-profiles.json (native tools plus selected hosted services)
      --token-file string          private 0600 file path for the HTTP bearer token record (requires --http)

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh mcp [command] --help" for more information about a command.
```

## mesh mcp connect

```text
Run a stateless MCP 2026-07-28 stdio server on this machine that forwards
unqualified tool, prompt, and resource names directly to one hosted MCP server
on HOST over an authenticated Mesh H3 stream.

stdout carries only newline-delimited JSON-RPC MCP messages; diagnostics, help,
and startup errors go to stderr. Only Mesh is needed on this client.

Usage:
  mesh mcp connect HOST/NAME [flags]

Examples:
mesh mcp connect pc/repo-tools --identity codex
mesh mcp connect local/browser --identity claude --instance mi1.HOST.SVC.INST

Flags:
  -h, --help              help for connect
      --identity string   local application identity created with mesh mcp identity create NAME (required)
      --instance string   optional instance handle (mi1.<hostId>.<serviceId>.<instanceId>) or instance ID

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp identity

```text
Manage per-agent application identities

Usage:
  mesh mcp identity [flags]
  mesh mcp identity [command]

Available Commands:
  create      Create a private application credential for one agent frontend
  ls          List local application identities without their secrets

Flags:
  -h, --help   help for identity

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh mcp identity [command] --help" for more information about a command.
```

## mesh mcp identity create

```text
Create a private application credential for one agent frontend

Usage:
  mesh mcp identity create NAME [flags]

Flags:
  -h, --help   help for create
      --json   write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp identity ls

```text
List local application identities without their secrets

Usage:
  mesh mcp identity ls [flags]

Aliases:
  ls, list

Flags:
  -h, --help   help for ls
      --json   write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp logs

```text
Print a hosted MCP server instance's recent stderr

Usage:
  mesh mcp logs [HOST/]NAME [flags]

Flags:
  -h, --help              help for logs
      --identity string   act as this local application identity
      --instance string   instance handle or ID, required when the server has several instances
      --json              write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp ls

```text
Query saved Mesh hosts concurrently under a 2-second budget and list their
hosted MCP servers without waking offline hosts or starting stopped services.
Unreachable hosts retain eligible private cached entries for up to 24 hours
(marked stale, then expired); authorization denials and pin changes discard
cached entries immediately.

Usage:
  mesh mcp ls [flags]

Aliases:
  ls, list

Flags:
      --cached             read private cached catalog entries without dialing hosts
  -h, --help               help for ls
      --host string        limit listing to one machine name, exact host ID, or local
      --identity string    scope visible instances and private catalog cache to this application identity
      --json               write one agent result
      --timeout duration   hard deadline for fleet queries (100ms to 30s) (default 2s)

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp profile

```text
Manage aggregate gateway profiles in mcp-profiles.json

Usage:
  mesh mcp profile [flags]
  mesh mcp profile [command]

Available Commands:
  add         Create a profile or select one hosted service in it
  alias       Rename a service's alias; exposed names do not change
  repair      Rebind an entry whose service was removed and published again, by name
  rm          Remove a profile, or one service from it
  show        Show gateway profiles and the state of each selected service

Flags:
  -h, --help   help for profile

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh mcp profile [command] --help" for more information about a command.
```

## mesh mcp profile add

```text
Create a profile or select one hosted service in it

Usage:
  mesh mcp profile add NAME [HOST/SERVICE] [flags]

Flags:
      --alias string             display alias for the service (default: its name)
      --exclude stringArray      hide upstream names matching this pattern (repeatable)
      --expected-revision uint   fail unless mcp-profiles.json is still at this revision
  -h, --help                     help for add
      --identity string          read the host catalog as this application identity
      --include stringArray      expose only upstream names matching this pattern (repeatable)
      --instance string          pin an instance ID or mi1 handle
      --json                     write one agent result
      --native string            native groups: all, none, or a comma list of hosts,contexts,exec,sessions,services,terminal,files,fleet

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp profile alias

```text
Rename a service's alias; exposed names do not change

Usage:
  mesh mcp profile alias NAME OLD NEW [flags]

Flags:
      --expected-revision uint   fail unless mcp-profiles.json is still at this revision
  -h, --help                     help for alias
      --json                     write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp profile repair

```text
Rebind an entry whose service was removed and published again, by name

Usage:
  mesh mcp profile repair NAME ALIAS [flags]

Flags:
      --confirm                  bind the entry to the service now published under its recorded name
      --expected-revision uint   fail unless mcp-profiles.json is still at this revision
  -h, --help                     help for repair
      --identity string          read the host catalog as this application identity
      --json                     write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp profile rm

```text
Remove a profile, or one service from it

Usage:
  mesh mcp profile rm NAME [ALIAS] [flags]

Flags:
      --expected-revision uint   fail unless mcp-profiles.json is still at this revision
  -h, --help                     help for rm
      --json                     write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp profile show

```text
Show gateway profiles and the state of each selected service

Usage:
  mesh mcp profile show [NAME] [flags]

Flags:
      --cached                   show the file only, without asking hosts for service states
      --expected-revision uint   fail unless mcp-profiles.json is still at this revision
  -h, --help                     help for show
      --identity string          read the host catalog as this application identity
      --json                     write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp publish

```text
Publish stores a server definition on the target host (this machine by default).
A stdio server runs COMMAND; an HTTP server is reached at --url, optionally started
with --run. Each application caller gets its own isolated instance unless --shared is given.

Usage:
  mesh mcp publish NAME [flags] [-- COMMAND [ARG...]]

Examples:
mesh mcp publish repo-tools --cwd /work/app -- ./repo-mcp
mesh mcp publish search --host pc --url http://127.0.0.1:8080/mcp --shared

Flags:
      --account-label string        non-secret name of the host account, shown to callers
      --account-mode string         none, or host-account when the server uses the host's credentials
      --cwd string                  working directory on the target host for a stdio server
      --env stringArray             NAME=VALUE for the server's environment; repeat as needed
      --header-secret stringArray   HEADER=/absolute/file sent to the HTTP server
  -h, --help                        help for publish
      --host string                 target host (machine name, exact host ID, or local; defaults to local)
      --idle duration               stop an on-demand instance after this long unused (default 15m)
      --json                        write one agent result
      --kept                        keep the server running instead of starting it on demand
      --ready-timeout duration      how long the server has to answer server/discover (default 30s)
      --run string                  command that starts the HTTP server at --url
      --run-cwd string              working directory for --run
      --secret-env stringArray      NAME=/absolute/file read into NAME at launch; the file must be mode 0600
      --shared                      give every caller one shared instance
      --url string                  loopback Streamable HTTP endpoint of an HTTP server

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp restart

```text
Replace a hosted MCP server instance's process

Usage:
  mesh mcp restart [HOST/]NAME [flags]

Flags:
  -h, --help              help for restart
      --identity string   act as this local application identity
      --instance string   instance handle or ID; without it, start and restart use your own instance and stop stops all of them
      --json              write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp rm

```text
Remove a hosted MCP server and stop its processes

Usage:
  mesh mcp rm [HOST/]NAME [flags]

Flags:
  -h, --help   help for rm
      --json   write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp show

```text
Show one hosted MCP server and its instances

Usage:
  mesh mcp show [HOST/]NAME [flags]

Flags:
  -h, --help              help for show
      --identity string   act as this local application identity
      --json              write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp start

```text
Start a hosted MCP server's instance now

Usage:
  mesh mcp start [HOST/]NAME [flags]

Flags:
  -h, --help              help for start
      --identity string   act as this local application identity
      --instance string   instance handle or ID; without it, start and restart use your own instance and stop stops all of them
      --json              write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp stop

```text
Stop a hosted MCP server's processes and keep its definition

Usage:
  mesh mcp stop [HOST/]NAME [flags]

Flags:
  -h, --help              help for stop
      --identity string   act as this local application identity
      --instance string   instance handle or ID; without it, start and restart use your own instance and stop stops all of them
      --json              write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh mcp token

```text
Manage bearer tokens for the native loopback MCP HTTP server

Usage:
  mesh mcp token [flags]
  mesh mcp token [command]

Available Commands:
  rotate      Rotate the bearer token for a running loopback MCP HTTP listener

Flags:
  -h, --help   help for token

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh mcp token [command] --help" for more information about a command.
```

## mesh mcp token rotate

```text
Replace the bearer token stored for one running mesh mcp --http listener and
retire all active streams authenticated with the previous token. The token
value is written only to the private token file and is never printed or logged.

Usage:
  mesh mcp token rotate [flags]

Flags:
  -h, --help              help for rotate
      --http string       exact loopback host:port of the running listener
      --identity string   local application identity bound to the listener
      --json              write one agent result

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh private-names

```text
Manage private DNS and wildcard certificates

USAGE

  mesh private-names [command] [--flags]

COMMANDS

  reconcile [--flags]  Reconcile DNS, renew, and distribute one certificate

FLAGS

  -h --help            Help for private-names
  --leave-key          Key that leaves all nested sessions, or none (ctrl+^)
  --privacy            Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh private-names reconcile

```text
Reconcile DNS, renew, and distribute one certificate

USAGE

  mesh private-names reconcile [--flags]

FLAGS

  --accept-tos  Accept the Let's Encrypt subscriber agreement
  --config      Private-Name JSON config file
  --force       Issue a new certificate even when the current one is fresh
  -h --help     Help for reconcile
  --leave-key   Key that leaves all nested sessions, or none (ctrl+^)
  --live        Use the isolated Let's Encrypt production environment
  --privacy     Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --staging     Use the isolated Let's Encrypt staging environment
```

## mesh shell-init

```text
Print opt-in directory and history recovery hooks

USAGE

  mesh shell-init bash|zsh [--flags]

FLAGS

  --agents     Also wrap native codex and claude commands for conversation recovery
  -h --help    Help for shell-init
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh skill

```text
Show and manage the embedded Mesh agent skill

USAGE

  mesh skill [command] [--flags]

COMMANDS

  install [--flags]    Install the skill into an explicitly selected skills root
  show [--flags]       Print exact embedded skill bytes without reading local configuration
  status [--flags]     Inspect bundle files and registration without changing them
  uninstall [--flags]  Remove unchanged managed files and unregister one root
  update [--flags]     Update managed roots and preserve user-modified files

FLAGS

  -h --help            Help for skill
  --json               Write one agent result
  --leave-key          Key that leaves all nested sessions, or none (ctrl+^)
  --privacy            Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh skill install

```text
Install the skill into an explicitly selected skills root

USAGE

  mesh skill install [--flags]

FLAGS

  --dir        Existing skills root; omit for all registered roots on status or update
  -h --help    Help for install
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh skill show

```text
Print exact embedded skill bytes without reading local configuration

Usage:
  mesh skill show [flags]

Flags:
      --file string   embedded file, such as references/commands.md
  -h, --help          help for show

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh skill status

```text
Inspect bundle files and registration without changing them

USAGE

  mesh skill status [--flags]

FLAGS

  --dir        Existing skills root; omit for all registered roots on status or update
  -h --help    Help for status
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh skill uninstall

```text
Remove unchanged managed files and unregister one root

USAGE

  mesh skill uninstall [--flags]

FLAGS

  --dir        Existing skills root; omit for all registered roots on status or update
  -h --help    Help for uninstall
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh skill update

```text
Update managed roots and preserve user-modified files

USAGE

  mesh skill update [--flags]

FLAGS

  --dir        Existing skills root; omit for all registered roots on status or update
  -h --help    Help for update
  --json       Write one agent result
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update

```text
Review and update Mesh on this machine or your saved fleet

Usage:
  mesh update [flags]
  mesh update [command]

Available Commands:
  adopt       Bring an existing installation into automatic updates
  cancel      Stop pending updates; issued activation grants may still finish
  config      Deliver explicitly approved feed and domain settings
  fleet       Read update status from every machine in your saved fleet
  policy      Manage automatic updates on this machine
  retry       Retry unfinished targets at the originally approved release
  status      Show persisted fleet update progress
  trust       Authorize an update administrator on this machine

Flags:
      --all                  update the machines in your saved fleet
      --check                check published and installed versions without installing
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --fleet string         use an explicit fleet manifest
  -h, --help                 help for update
      --host stringArray     update a named machine; may repeat
      --json                 write structured results
      --local                update this machine only
      --version string       exact published release tag, or latest (default "latest")
      --yes                  approve the selected scope without prompting

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh update [command] --help" for more information about a command.
```

## mesh update adopt

```text
Bring an existing installation into automatic updates

Usage:
  mesh update adopt [command]

Available Commands:
  abandon     Withdraw an adoption that has not reached any remote or activated this host
  start       Start one durable adoption from a verified release executable
  status      Read this host's durable adoption progress

Flags:
  -h, --help   help for adopt

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh update adopt [command] --help" for more information about a command.
```

## mesh update adopt abandon

```text
Withdraw an adoption that has not reached any remote or activated this host

Usage:
  mesh update adopt abandon [flags]

Flags:
  -h, --help   help for abandon

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update adopt start

```text
Start one durable adoption from a verified release executable

Usage:
  mesh update adopt start [--notice -- COMMAND ARG...] [flags]

Flags:
      --canary stringArray     host identity that adopts first; may repeat
      --fleet string           fleet manifest to adopt
  -h, --help                   help for start
      --last string            local host identity that adopts last (default "LOCAL")
      --local                  receive this host's pinned adoption input on stdin
      --notice                 run the argv after -- before the last host activates
      --notice-lead duration   time between successful notice and local activation (default 30m0s)
      --release string         exact signed activation release
      --ssh stringArray        system SSH alias as ID=ALIAS; may repeat

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update adopt status

```text
Read this host's durable adoption progress

Usage:
  mesh update adopt status [flags]

Flags:
  -h, --help   help for status

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update cancel

```text
Stop pending updates; issued activation grants may still finish

Usage:
  mesh update cancel [RUN] [flags]

Flags:
  -h, --help    help for cancel
      --local   read or control this machine's approved update target

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update config

```text
Deliver explicitly approved feed and domain settings

Usage:
  mesh update config [command]

Available Commands:
  allow       Allow selected configuration groups from an exact coordinator
  apply       Approve the exact verified proposal for durable delivery
  cancel      Stop new dispatch intentions; earlier issued work may still finish
  preview     Capture exact targets, selected values and recipient grants without delivery
  retry       Explicitly retry the same approver's unchanged configuration
  revoke      Withdraw this account's configuration grant
  status      Show saved, issued and actual dashboard observations separately

Flags:
  -h, --help   help for config

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh update config [command] --help" for more information about a command.
```

## mesh update config allow

```text
Allow selected configuration groups from an exact coordinator

Usage:
  mesh update config allow COORDINATOR_ID [flags]

Flags:
      --domain-policy            allow primary domain and complete aliases; preserve existing accepted domains
      --expected-revision uint   expected target-local configuration grant revision
  -h, --help                     help for allow
      --usage-feed               allow dashboard usage-feed URL delivery

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update config apply

```text
Approve the exact verified proposal for durable delivery

Usage:
  mesh update config apply [flags]

Flags:
      --command-id string   administrative identity to reuse after response loss
      --file string         exact selection or previously returned proposal JSON file
  -h, --help                help for apply
      --yes                 approve the exact verified proposal

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update config cancel

```text
Stop new dispatch intentions; earlier issued work may still finish

Usage:
  mesh update config cancel CHANGE_ID [flags]

Flags:
      --command-id string        administrative identity to reuse after response loss
      --expected-revision uint   expected configuration administrative revision
  -h, --help                     help for cancel
      --retire                   local coordinator only: cancel, settle and archive without taking ownership

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update config preview

```text
Capture exact targets, selected values and recipient grants without delivery

Usage:
  mesh update config preview [flags]

Flags:
      --file string   exact selection or previously returned proposal JSON file
  -h, --help          help for preview

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update config retry

```text
Explicitly retry the same approver's unchanged configuration

Usage:
  mesh update config retry CHANGE_ID [flags]

Flags:
      --command-id string        administrative identity to reuse after response loss
      --expected-revision uint   expected configuration administrative revision
  -h, --help                     help for retry

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update config revoke

```text
Withdraw this account's configuration grant

Usage:
  mesh update config revoke [flags]

Flags:
      --expected-revision uint   expected target-local configuration grant revision
  -h, --help                     help for revoke

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update config status

```text
Show saved, issued and actual dashboard observations separately

Usage:
  mesh update config status [CHANGE_ID] [flags]

Flags:
  -h, --help   help for status

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update fleet

```text
Read update status from every machine in your saved fleet

Usage:
  mesh update fleet [flags]

Flags:
  -h, --help   help for fleet

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update policy

```text
Manage automatic updates on this machine

Usage:
  mesh update policy [command]

Available Commands:
  forget      Release update history retained for one machine
  off         Stop granting automatic updates on this machine
  set         Enable automatic updates on this machine
  show        Show this machine's update policy

Flags:
  -h, --help   help for policy

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh update policy [command] --help" for more information about a command.
```

## mesh update policy forget

```text
Release update history retained for one machine

Usage:
  mesh update policy forget DEPENDENT_ID [flags]

Flags:
  -h, --help   help for forget

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update policy off

```text
Stop granting automatic updates on this machine

Usage:
  mesh update policy off [flags]

Flags:
      --expected-revision uint   policy revision reported by policy show
  -h, --help                     help for off

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update policy set

```text
Enable automatic updates on this machine

Usage:
  mesh update policy set [flags]

Flags:
      --after stringArray        wait for a named machine before updating; may repeat
      --expected-revision uint   policy revision reported by policy show
  -h, --help                     help for set
      --soak duration            time to wait after each predecessor updates

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update policy show

```text
Show this machine's update policy

Usage:
  mesh update policy show [flags]

Flags:
  -h, --help   help for show

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update retry

```text
Retry unfinished targets at the originally approved release

Usage:
  mesh update retry [RUN] [flags]

Flags:
  -h, --help    help for retry
      --local   read or control this machine's approved update target

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update status

```text
Show persisted fleet update progress

Usage:
  mesh update status [RUN] [flags]

Flags:
  -h, --help    help for status
      --local   read or control this machine's approved update target

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update trust

```text
Authorize an update administrator on this machine

Usage:
  mesh update trust ID [flags]

Flags:
  -h, --help       help for trust
      --if-empty   enroll only when no administrator policy exists

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh version

```text
Show the executing Mesh build

Usage:
  mesh version [flags]

Flags:
  -h, --help       help for version
      --json       write build metadata as JSON
      --licenses   print the bundled Brotli notice

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```
