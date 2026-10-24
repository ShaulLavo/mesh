# Troubleshooting

Each entry starts with what you see. The error text is copied from Mesh 0.1.251 output or source.

## `"pc" is neither a declared machine name, exact host ID nor a session ID`

Mesh treats a bare word as a host or a session, and it knows no host or session called `pc`. Either you have not adopted that machine, or its name differs from the one you typed.

1. Run `mesh` with no arguments, or `mesh ls`, and read the host names.
2. If the machine is missing, run `mesh add user@pc`.
3. If it is present under another name, use that name. Each machine owns its name. See [Machine names](reference/machine-names.md).

## `cli: daemon unavailable: dial unix .../daemon.sock: connect: no such file or directory`

No daemon runs on this machine. `mesh local` works without one, but listing hosts, serving and being adopted by other machines need it.

Installing from Homebrew gives you the `mesh` command and no daemon. Install the service:

```bash
mesh daemon install
```

If you installed with `scripts/install.sh` the service already exists. Check it:

```bash
systemctl --user status mesh.service                   # Linux
launchctl print "gui/$(id -u)/dev.shaulavo.mesh"       # macOS
```

## `bootstrap ssh_connect: dial SSH nowhere:22: ...; fix: run ssh user@host and check the host name, SSH port, and network route`

`mesh add` reaches a machine through system SSH. Run `ssh user@host` yourself first. When that works, `mesh add user@host` can too. [Reach sessions over SSH](guides/remote-access.md) covers the separate Mesh SSH listener on port 2222.

## The Linux installer stops with `gum is required` or `Gum v2 is required`

`scripts/install.sh` asks for confirmation through [gum](https://github.com/charmbracelet/gum) v2. Install gum, or skip the prompt:

```bash
curl -fsSL https://raw.githubusercontent.com/ShaulLavo/mesh/main/scripts/install.sh | MESH_INSTALL_YES=1 sh
```

If it stops with `cannot enable user lingering`, run the `loginctl enable-linger USER` command that the message prints. The daemon is a user service and needs lingering to run when you are logged out.

## `macOS releases require Apple silicon (arm64)`

Mesh publishes macOS builds for Apple silicon only. Intel Macs are not supported. See the platform table in the [README](../README.md#status).

## macOS says the binary cannot be opened, or asks for folder access again

Releases are ad-hoc signed. The Homebrew cask clears the quarantine attribute. For a binary you downloaded by hand, clear it yourself:

```bash
xattr -d com.apple.quarantine ./mesh
```

Folder-access approvals can need renewing after an update for the same reason. [Run Mesh on macOS](guides/macos.md) has the steps.

## `session ETRG is already exited`

`mesh kill` ends a running session. This one has already ended. `mesh ls` hides ended sessions and prints `N ended session hidden; mesh ls --all lists them`. Run `mesh ls --all` to see them, and `mesh rm ID` to forget one.

## After a reboot a session shows `interrupted`

A reboot kills every process on the host, and Mesh does not pretend otherwise. The session keeps its directory, shell history and last screen so you can relaunch. See [Recover a workspace](guides/recovery.md).

## `ctrl+]` does nothing useful, or an editor needs that key

`ctrl+]` is the default detach key. Programs that use it (the `vim` tag jump) never see it. Pick another key or turn the detach key off for one run:

```bash
mesh pc --detach-key ctrl+a
mesh pc --detach-key none
```

`--raw` also passes every byte through. Inside nested sessions `ctrl+]` detaches the innermost one and `ctrl+^` leaves them all. See [ADR 007](adr/007-innermost-detach-key.md).

## `Mesh cannot identify this terminal, so it cannot remember sessions for it`

`mesh back` needs to know which terminal tab it runs in, and yours does not say. Name it yourself:

```bash
export MESH_TERMINAL_ID=work
```

## Another client took my session

A session has one attached client. When a second client attaches, Mesh detaches the first. This is deliberate: [ADR 003](adr/003-single-attacher.md) records why. Reattach with `mesh pc -r` when the other client is done.
