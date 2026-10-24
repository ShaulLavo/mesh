# 005. Mesh device keys authorize network controls

Status: accepted
Date: 2026-10-03

## Context

Tailscale decides which machines can reach a host, but reachability is not permission to run commands as the daemon's account. Mesh first used one Ed25519 host identity for both WebSocket and SSH, so there was one key to trust and rotate. Network controls still needed their own authorization.

## Decision

The terminal WebSocket carries TLS 1.3 inside its binary stream. The certificate wraps the existing Ed25519 Mesh identity. Both peers prove possession of their key. The client checks an owner-controlled destination pin, and the daemon checks its approved `authorized_keys`. CA roots, certificate names, discovery and Tailnet membership grant no device access. TLS session tickets are disabled. The SSH host key is the same identity exported in OpenSSH format, so Mesh has no second keypair.

## Alternatives considered

- **Trust the Tailnet.** Anyone on the tailnet could control every host. Tailscale's boundary is useful and is kept, but it is not a grant.
- **A second SSH keypair.** Two keys mean two things to trust and two answers to "is this really my Pi".

## Consequences

Network connections and terminal records pay for cryptographic work. Revoking a grant denies new admissions and retires active SSH and WebSocket connections. Detached workers keep running. Existing update administrators keep update-only authority until you enroll them for full control. See [Device access](../concepts/device-access.md).
