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

## mesh session

```text
Read sessions and run keyed detached commands

USAGE

  mesh session [command] [--flags]

COMMANDS

  create -- command [args...] [--flags]  Start a keyed detached PTY command on an online host
  inspect SESSION_ID [--flags]           Read live or saved session details without attaching
  list [--flags]                         List one host's sessions with bounded pages
  request [--flags]                      Look up a retained creation receipt without launching work

FLAGS

  -h --help                              Help for session
  --json                                 Write one agent result
  --leave-key                            Key that leaves all nested sessions, or none (ctrl+^)
  --privacy                              Mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh session create

```text
Start a keyed detached PTY command on an online host

USAGE

  mesh session create -- command [args...] [--flags]

FLAGS

  --cols         Initial terminal width (80)
  --cwd          Absolute clean working directory on the target
  -h --help      Help for create
  --host         Machine name or exact host ID, required
  --json         Write one agent result, required
  --kind         Explicit session kind, currently pty
  --leave-key    Key that leaves all nested sessions, or none (ctrl+^)
  --privacy      Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --request-key  Saved logical request key, required
  --rows         Initial terminal height (24)
  --term         Target terminal type (dumb)
  --timeout      Hard deadline from 100ms to 60s (40s)
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
  --json       Write one agent result, required
  --leave-key  Key that leaves all nested sessions, or none (ctrl+^)
  --preview    Include a control-free preview, at most 80 by 24
  --privacy    Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --timeout    Hard deadline for the read, from 100ms to 60s (5s)
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

## mesh session request

```text
Look up a retained creation receipt without launching work

USAGE

  mesh session request [--flags]

FLAGS

  -h --help      Help for request
  --host         Machine name or exact host ID, required
  --json         Write one agent result, required
  --leave-key    Key that leaves all nested sessions, or none (ctrl+^)
  --privacy      Mask personal data for recording and screen sharing (MESH_PRIVACY)
  --request-key  Saved logical request key, required
  --timeout      Hard deadline from 100ms to 60s (5s)
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
  automatic   Manage exact saved-fleet unattended update consent
  cancel      Stop pending updates; issued activation grants may still finish
  config      Deliver explicitly approved usage-feed settings
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

## mesh update automatic

```text
Manage exact saved-fleet unattended update consent

Usage:
  mesh update automatic [command]

Available Commands:
  acknowledge-stop Acknowledge a settled run or disabled pre-run policy stop; stay disabled
  authorize        Bind this account to an exact coordinator, fleet and epoch
  disable          Stop new automatic dispatch; issued work may still finish
  enable           Enable exact saved-fleet release tracking; wait for local recipient consent
  revoke           Withdraw this account's unattended binary consent
  status           Show policy, enrollment, stop, continuation and pinned run status

Flags:
  -h, --help   help for automatic

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)

Use "mesh update automatic [command] --help" for more information about a command.
```

## mesh update automatic authorize

```text
Bind this account to an exact coordinator, fleet and epoch

Usage:
  mesh update automatic authorize COORDINATOR_ID [flags]

Flags:
      --epoch uint               exact pending coordinator enrollment epoch
      --expected-revision uint   expected local recipient consent revision
      --fleet string             exact saved fleet file
  -h, --help                     help for authorize

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update automatic disable

```text
Stop new automatic dispatch; issued work may still finish

Usage:
  mesh update automatic disable [flags]

Flags:
      --command-id string        exact administrative command identity for response-loss replay
      --expected-revision uint   expected coordinator policy revision
  -h, --help                     help for disable

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update automatic enable

```text
Enable exact saved-fleet release tracking; wait for local recipient consent

Usage:
  mesh update automatic enable [flags]

Flags:
      --command-id string        exact administrative command identity for response-loss replay
      --expected-revision uint   expected coordinator policy revision
  -h, --help                     help for enable
      --take-ownership           local coordinator recovery after disabled, settled stop acknowledgement
      --yes                      approve unattended updates for the coordinator's exact saved fleet

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update automatic revoke

```text
Withdraw this account's unattended binary consent

Usage:
  mesh update automatic revoke [flags]

Flags:
      --expected-revision uint   expected local recipient consent revision
  -h, --help                     help for revoke

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update automatic status

```text
Show policy, enrollment, stop, continuation and pinned run status

Usage:
  mesh update automatic status [flags]

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
  mesh update cancel RUN [flags]

Flags:
      --command-id string               exact administrative command identity for response-loss replay
      --expected-policy-revision uint   expected automatic policy revision; required for automatic runs
  -h, --help                            help for cancel

Global Flags:
      --coordinator string   adopted host holding the operation; defaults to this machine
      --details              show release identifiers, selected machines, and update checks
      --json                 write structured results
      --leave-key string     key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy              mask personal data for recording and screen sharing (MESH_PRIVACY)
```

## mesh update config

```text
Deliver explicitly approved usage-feed settings

Usage:
  mesh update config [command]

Available Commands:
  allow       Allow usage-feed delivery from an exact coordinator
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
Allow usage-feed delivery from an exact coordinator

Usage:
  mesh update config allow COORDINATOR_ID [flags]

Flags:
      --expected-revision uint   expected target-local configuration grant revision
  -h, --help                     help for allow
      --usage-feed               allow only dashboard usage-feed URL delivery

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
  mesh update config status CHANGE_ID [flags]

Flags:
  -h, --help   help for status

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
      --command-id string               exact administrative command identity for response-loss replay
      --expected-policy-revision uint   expected automatic policy revision; required for automatic runs
  -h, --help                            help for retry

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
  -h, --help       help for version
      --json       write build metadata as JSON
      --licenses   print the bundled Brotli notice

Global Flags:
      --leave-key string   key that leaves all nested sessions, or none (default "ctrl+^")
      --privacy            mask personal data for recording and screen sharing (MESH_PRIVACY)
```
