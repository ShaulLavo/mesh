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

## The cost

TLS adds cryptographic work to network connections and terminal records. Update administrators keep update-only authority until you enroll them for full daemon-account control. [ADR 005](../adr/005-mesh-keys-authorize-controls.md) records why Mesh uses its own keys instead of Tailnet membership.
