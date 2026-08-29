# T08 — `mesh add shaul@pc`

**Status:** not started · **Blocked by:** T04, T06 · **Owns:** `internal/bootstrap/`,
`scripts/install/`

## Goal

One command turns an SSH-reachable machine into a Mesh host, after which SSH
disappears behind the curtain.

Steps: detect OS and architecture · upload or fetch the matching binary · install
a systemd unit (Linux) or launchd plist (macOS) · start it · generate and record
the Mesh identity · discover the Tailscale name and address · verify the WebSocket
connection end to end · save the host locally.

## The part that decides whether this is good

Failure diagnostics. Every step above has a common failure with a specific fix,
and the difference between a delightful tool and an infuriating one is whether the
error says which step failed and what to do. Enumerate them explicitly: no SSH
auth, wrong arch, no systemd, no user lingering, Tailscale not logged in, port
blocked, clock skew. Each gets a named error and a suggested next command.

`golang.org/x/crypto/ssh` for the transport, Huh v2 for the interactive parts.

## Acceptance

- Bootstrap against a local container or VM for Linux/systemd, from nothing to a
  verified WebSocket connection, in one command.
- Re-running `mesh add` on an already-configured host is idempotent and says so.
- Each enumerated failure mode has a test or a documented manual reproduction, and
  produces its named error rather than a raw SSH or exec error.
