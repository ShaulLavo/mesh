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
| [`mesh gc`](#mesh-gc) | Reclaim memory from sessions left detached and idle |
| [`mesh hibernate`](#mesh-hibernate) | Stop an idle agent now; its conversation resumes on the next attach |
| [`mesh kill`](#mesh-kill) | End a session and wait until it is gone |
| [`mesh local`](#mesh-local) | Start or resume a session on this host |
| [`mesh logs`](#mesh-logs) | Print recent terminal output without attaching |
| [`mesh ls`](#mesh-ls) | List sessions across known hosts |
| [`mesh recover`](#mesh-recover) | Recover a saved session and retain its previous output |
| [`mesh recovery-command`](#mesh-recovery-command) | Save an explicit restart command |
| [`mesh rm`](#mesh-rm) | Forget finished sessions and delete what they left behind |
| [`mesh sig`](#mesh-sig) | Send a signal to a session process group |
| [`mesh add`](#mesh-add) | Install Mesh on an SSH-reachable host |
| [`mesh cp`](#mesh-cp) | Copy files or directories through an authenticated Mesh connection |
| [`mesh dashboard`](#mesh-dashboard) | Watch machine usage, sessions, and services |
| [`mesh fleet`](#mesh-fleet) | Share connections with explicitly trusted devices |
| [`mesh rename`](#mesh-rename) | Change the destination machine's name |
| [`mesh ssh`](#mesh-ssh) | Open a shell or command that ends when the connection closes |
| [`mesh wake`](#mesh-wake) | Wake a configured host |
| [`mesh app`](#mesh-app) | Create and manage disposable websites |
| [`mesh serve`](#mesh-serve) | Publish or list services from adopted hosts |
| [`mesh unserve`](#mesh-unserve) | Remove one private service |
| [`mesh daemon`](#mesh-daemon) | Run this host's coordinator |
| [`mesh device`](#mesh-device) | Approve device keys for this daemon's OS account |
| [`mesh private-names`](#mesh-private-names) | Manage private DNS and wildcard certificates |
| [`mesh shell-init`](#mesh-shell-init) | Print opt-in directory and history recovery hooks |
| [`mesh update`](#mesh-update) | Review and update Mesh on this machine or your saved fleet |
| [`mesh version`](#mesh-version) | Show the executing Mesh build |

## mesh

```text
Sessions live on the host that runs them, not on the connection.
Close your laptop, lose wifi, or quit mesh, and the command keeps
running. Reattach from anywhere on your tailnet.

A bare name is a host or a session, so the commands you use most are
not subcommands at all.

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
  gc [--flags]                                          Reclaim memory from sessions left detached and idle
  hibernate session...                                  Stop an idle agent now; its conversation resumes on the next attach
  kill session                                          End a session and wait until it is gone
  local [-- command...] [--flags]                       Start or resume a session on this host
  logs session [--flags]                                Print recent terminal output without attaching
  ls [--flags]                                          List sessions across known hosts
  recover SESSION [--flags]                             Recover a saved session and retain its previous output
  recovery-command SESSION -- PROGRAM ARG... [--flags]  Save an explicit restart command
  rm session...                                         Forget finished sessions and delete what they left behind
  sig session signal                                    Send a signal to a session process group

HOSTS

  add host [user@] [--flags]                            Install Mesh on an SSH-reachable host
  cp SOURCE DESTINATION [--flags]                       Copy files or directories through an authenticated Mesh connection
  dashboard [--flags]                                   Watch machine usage, sessions, and services
  fleet [command]                                       Share connections with explicitly trusted devices
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
  private-names [command]                               Manage private DNS and wildcard certificates
  shell-init bash|zsh [--flags]                         Print opt-in directory and history recovery hooks
  update [command] [--flags]                            Review and update Mesh on this machine or your saved fleet
  version [--flags]                                     Show the executing Mesh build

FLAGS

  --detach-key                                          Key that detaches, such as ctrl+] or none
  -h --help                                             Help for mesh
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
  --timeout    Maximum wait for each host catalog reply after authenticated setup (750ms)
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

  -h --help    Help for ssh
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
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
  mesh serve ls
  mesh serve list
  mesh serve start ROUTE
  mesh serve stop ROUTE

COMMANDS

  label ROUTE NAME [--flags]  Set the display name of an existing service
  ls [--flags]                List live and cached services
  start ROUTE [--flags]       Start an on-demand route now and wait until it is ready
  stop ROUTE [--flags]        Stop an on-demand route now; the next connection starts it again

FLAGS

  --at                        Route path, such as /blog
  --cwd                       Directory --run starts in (default: this directory)
  --env                       Key=Value added to the environment of --run (repeatable)
  --files                     Enable directory listings
  -h --help                   Help for serve
  --idle                      Stop --run after no connection has been open this long (15m0s)
  --isolate                   Send cross-origin isolation headers so the page can use SharedArrayBuffer
  --label                     Display name shown in service lists
  --leave-key                 Key that leaves all nested sessions, or none (ctrl+^)
  --listen                    Public=Upstream: proxy 127.0.0.1:PUBLIC on the host to 127.0.0.1:UPSTREAM (repeatable)
  --privacy                   Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --private-host              Private hostname at the root, using a label or a configured deployment hostname
  --ready-timeout             How long a starting --run holds connections before failing them (1m0s)
  --run                       Command that serves the port; started on the first connection, stopped when idle
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
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for live host queries (2s)
```

## mesh serve start

```text
Start an on-demand route now and wait until it is ready

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
Stop an on-demand route now; the next connection starts it again

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

## mesh update

```text
Review and update Mesh on this machine or your saved fleet

Usage:
  mesh update [flags]
  mesh update [command]

Available Commands:
  cancel      Stop pending updates; issued activation grants may still finish
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

## mesh update cancel

```text
Stop pending updates; issued activation grants may still finish

Usage:
  mesh update cancel RUN [flags]

Flags:
  -h, --help   help for cancel

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
  mesh update retry RUN [flags]

Flags:
  -h, --help   help for retry

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
  -h, --help   help for status

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
  -h, --help   help for version
      --json   write build metadata as JSON

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```
