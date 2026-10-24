# 004. SSH is a front door on port 2222, not the transport

Status: accepted
Date: 2026-08-30

## Context

Mesh clients talk to each other over the WebSocket transport. But `ssh` is installed on every machine you will ever sit at, and Mesh is not: a work laptop you cannot install on, a phone, someone else's desktop. The system SSH server owns port 22 on most hosts and `mesh add` relies on it for bootstrap and recovery.

## Decision

Each host runs a Charm SSH server with public-key-only authentication. It serves the picker, sessions, `ls`, and read-only SFTP and SCP of served directories. It listens on port 2222 at each discovered Tailnet address. The installers pass `--ssh-port=2222` to the user service. The daemon's own default is off (`--ssh-port` zero).

## Alternatives considered

- **SSH as the transport between hosts.** It would reuse SSH authentication, but resume by byte offset is the point of the WebSocket transport, and SSH channels would give that up for authentication Mesh already has.
- **Take port 22.** A user service cannot bind a privileged port on a standard Linux host, and replacing the system server would remove the recovery path `mesh add` uses.
- **A native phone app.** It would have to reimplement attach, replay, resize and steal against a protocol that changes. A phone with Tailscale and an SSH client already gets all of that.

## Consequences

Any SSH client reaches a session. The cost is a second listening server on every host and a second authentication surface to keep correct. A stock client needs `-p 2222`, or a `~/.ssh/config` entry that sets the port, identity file and `IdentitiesOnly` once. See [Reach sessions over SSH](../guides/remote-access.md).
