# Device access

A Mesh host decides who may control it by device key, not by network position. This page explains the model. To approve a device, follow [Approve a device](../guides/approve-devices.md).

## What authenticates a connection

Network controls use mutual TLS 1.3 with each device's existing Mesh identity, an Ed25519 key. Both sides prove they hold their key. The client checks the destination key against a pin it saved when it enrolled. The daemon checks the client key against its approved `authorized_keys`.

Discovery supplies addresses and nothing else. Tailnet membership, CA roots and certificate names grant no device access. Tailscale still restricts who can reach the host, and Mesh adds its own check on top. Unix controls on the same machine rely on same-account socket permissions.

The TLS 1.3 session runs inside the WebSocket's binary stream and keeps session tickets off. The outer listener and the HTTP service URLs are unchanged.

## What an approved device can do

An approved device can run commands, inspect sessions and manage services as the OS account that runs the destination daemon. That is full control of the account, not a read-only view. Approving a daemon that runs as root needs an explicit acknowledgement (`--allow-root`).

## How a key gets pinned and approved

`mesh add user@host` obtains the destination's key through authenticated system SSH and approves this device for that destination's daemon account. When system SSH is unavailable, adoption needs an existing pin. An unknown or changed destination key requires re-enrollment through system SSH. A discovery result never replaces a saved key.

Approval and revocation run on the destination, through trusted system SSH or its local terminal. Compare the identity and SHA256 fingerprint through that trusted channel before you approve.

## What revocation does

Grant changes and frame admission share a lock, so frames admitted after a revocation are denied immediately. Work admitted earlier can finish. Connections bind the incarnation of the approved key line, so revoking and immediately reapproving a key retires the old attachments and permits new ones. Idle SSH and control connections close when the next 250 ms grant check sees the revocation.

Persistent sessions and their detached workers stay available to other approved devices. A `mesh ssh` command ends when its device grant is revoked, because that command is tied to its connection.

## Security notes

Checked against a v0.1.249 build on macOS.

- **Ports 7337 and 2222 listen on Tailscale addresses only.** The daemon binds the host's addresses in `100.64.0.0/10` and `fd7a:115c:a1e0::/48`. It skips the SSH listener when an address is outside those ranges. `lsof` on a running host showed both ports bound to the Tailscale IPv4 and IPv6 addresses and nothing on a LAN or public address. Do not forward them from a router or bind them elsewhere.
- **Reaching a port is not authority.** Anyone Tailscale lets reach the host can open a connection. Port 7337 at `/mesh` answers a plain HTTP request with `426 Upgrade Required`. The connection then needs mutual TLS with an approved Mesh key. Port 2222 accepts public keys only, refuses an unapproved key, caps open connections at 256, and drops a connection that has not authenticated within 30 seconds. Restrict who reaches these ports with Tailscale access controls.
- **HTTP service routes share port 7337.** Routes published with `mesh serve` are served on that port, and the serve handler does no Mesh key check. Treat anything you publish as readable by every device that can reach the host.
- **An approved key is full control of the account.** Another approved device can attach to any session on the host and see its replay ring, the recent output, including a secret printed there. Mesh does not scope approval to one session.
- **Command lines are visible to approved devices.** `mesh ls` and `ssh HOST ls` show each session's command, including arguments. Pass secrets through the environment or a file, not as arguments.
- **Guard `identity.key`.** `~/.local/state/mesh/identity.key` is the device's private key, and anyone holding it is that device. Revoke a lost device with `mesh device revoke` on each host. See [Approve a device](../guides/approve-devices.md#revoke-a-device).
- **Recordings and screen shares.** `mesh --privacy` masks Mesh's fleet views and withholds saved terminal screens and logs. An attached session still shows its program's output as-is. See [privacy mode](../guides/privacy.md).

## The cost

TLS adds cryptographic work to network connections and terminal records. Update administrators keep update-only authority until you enroll them for full daemon-account control. [ADR 005](../adr/005-mesh-keys-authorize-controls.md) records why Mesh uses its own keys instead of Tailnet membership.
