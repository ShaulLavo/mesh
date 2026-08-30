# T15 — SSH front door

**Status:** not started · **Blocked by:** T06 · **Owns:** `internal/sshd/`

## Goal

`mesh daemon` listens for SSH on the tailnet interface. Connecting with an
authorized key gets you a session that says hello and exits. That is the whole
task: the door, hinged and locked, with nothing behind it yet.

T16, T17 and T18 hang their handlers off this. Land it first and land it small.

## Responsibilities

1. **Host key from the Mesh identity.** Export the existing `internal/identity`
   ed25519 private key in OpenSSH format and use it as the SSH host key (D18).
   Do not generate a second keypair. The fingerprint a user sees on first connect
   must correspond to the same identity `mesh ls` shows for that host.
2. **Public key authentication only.** No passwords, no keyboard-interactive.
   Read the authorized set from the state directory. T08 writes it during
   bootstrap; until T08 lands, populating it by hand is fine and the tests should
   do exactly that.
3. **Listener scope.** Bind the tailnet interface, not `0.0.0.0`. Reuse T06's
   address discovery. A host with no tailnet address does not start an SSH
   listener and says so in the log rather than falling back to every interface.
4. **Middleware.** Use what Wish ships rather than reinventing it:
   `recover` outermost, then `logging`, `ratelimiter`, `accesscontrol`, and
   `activeterm` only on the handlers that need a PTY. One-shot exec must work
   without a PTY, so `activeterm` cannot be global.
5. **Lifecycle.** The SSH server starts and stops with the daemon and shares its
   shutdown context. Killing the daemon must not kill sessions, same as always:
   an SSH connection is a client, and clients are disposable.

## Shape

```go
package sshd

type Config struct {
    HostKey       ed25519.PrivateKey // from internal/identity
    AuthorizedKeys string            // path in the state directory
    Addr          string             // tailnet address from T06
}

// Serve runs until ctx is done. Handlers are registered by T16, T17 and T18.
func Serve(ctx context.Context, cfg Config, opts ...ssh.Option) error
```

## The part that needs care

This is a second authentication surface on every host. The Mesh protocol side is
protected by Tailscale and a 0700 socket directory; this one is protected by a
key file you have to get right.

- An unreadable or missing authorized_keys file means **refuse every connection**,
  not allow every connection. Test that explicitly.
- An authorized_keys file that is group or world writable is a refusal too.
- Do not log public keys of failed attempts at info level; a scan fills the disk.

## Acceptance

- Go tests using Wish's `testsession`: authorized key connects, unauthorized key
  is refused, no key is refused.
- Missing authorized_keys refuses rather than permits. Same for an unreadable one.
- The host key fingerprint matches the host identity reported by the daemon.
- The listener binds the tailnet address only. Assert it is not on loopback or
  `0.0.0.0`.
- Daemon shutdown closes the listener and does not touch running sessions.

## Out of scope

Sessions (T17), files (T16), tunnels (T18), and writing authorized_keys (T08).
