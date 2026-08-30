# T18 — Reverse tunnels through the edge

**Status:** not started · **Blocked by:** T13, T15 · **Owns:** `internal/tunnel/`

## Goal

```
ssh -R blog:80:localhost:3000 vps.mesh.shaulavo.dev
```

A dev server on the laptop in front of you, answering at a public name, gone the
moment you disconnect. The one capability Tailscale cannot provide, because it
needs a publicly routable machine and step 8 already put one there.

## The rule this must not break

The overview rules out arbitrary tunnels and that stays true. D20 defines what is
allowed, and every clause is a test:

- authenticated by an authorized key (T15);
- bound to a route the user **named** in the forward request, never derived;
- refused when another host already holds that name;
- refused when the name was not claimed under D15's public-exposure rules;
- unpublished the instant the SSH connection drops.

D14 is unaffected. Tunnelled HTTP is not terminal traffic, and terminal sessions
still go direct to the origin.

## Responsibilities

1. **Accept `tcpip-forward`.** `charmbracelet/ssh`'s `tcpip.go` implements it and
   ships an `ssh-remoteforward` example. Start there.
2. **Register with the edge.** A successful forward becomes a T13 route pointing
   at the SSH connection instead of at a Tailscale origin. T13 already refuses
   collisions; reuse that path rather than adding a second registry.
3. **Unregister on close.** Connection drop, network death, and daemon shutdown
   all unpublish the route. A stale route pointing at a dead channel is the
   failure mode that makes this feature untrustworthy.
4. **Bound it.** Cap concurrent forwards per key and bytes in flight per forward.
   The edge already rate limits by IP (T13); this is the second half, limiting
   the authenticated side.

## The part that needs care

This is the only feature where an authenticated user makes something publicly
reachable in one command, with no file on disk to review first. D15's whole point
is that nothing goes public without being named, and a shell one-liner is the
easiest place to lose that.

Refuse a forward whose name was not already claimed as a public route. Requiring
`mesh serve --public blog` first is the correct amount of friction, and it means
the confirmation D15 mandates happened at a moment when someone was reading.

## Acceptance

- End to end: `ssh -R` from a machine, request the public name, get the local
  server's response.
- Disconnect, then request again: 404 from the edge, not a hang and not a 502
  that names the machine.
- A second host requesting a held name is refused with a clear error.
- A forward naming an unclaimed route is refused.
- Concurrent forward and byte limits are enforced, with tests that hit them.
- Terminal traffic still does not transit the edge. Same assertion T13 makes.

## Out of scope

Local forwarding (`ssh -L`), which Tailscale already does better. UDP. Anything
that forwards to a destination other than the connecting client's own machine.
