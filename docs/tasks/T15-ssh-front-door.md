# T15 — SSH front door

**Status:** complete · **Blocked by:** nothing · **Owns:** `internal/sshd/`

## Goal

`mesh daemon` listens for SSH on the tailnet interface. An authorized key gets
a session that prints `mesh ssh ready` and exits. Sessions, files, and tunnels
remain separate tasks.

T16, T17 and T18 hang their handlers off this. Land it first and land it small.

## Responsibilities

1. **Host key from the Mesh identity.** Export the existing `internal/identity`
   ed25519 private key in OpenSSH format and use it as the SSH host key (D18).
   Do not generate a second keypair. The fingerprint a user sees on first connect
   must correspond to the same identity `mesh ls` shows for that host.
2. **Public key authentication only.** No passwords, no keyboard-interactive.
   Read `authorized_keys` from the daemon state directory. T08's Linux and macOS
   installers already create and update that file idempotently with mode `0600`.
   Consume that file directly; do not add another key store.
3. **Stock OpenSSH selects the same identity.** T08 authorizes the adopter's Mesh
   identity, which OpenSSH does not find in its default `~/.ssh/id_*` search.
   Document and test `-p 2222 -o IdentitiesOnly=yes -i STATE_DIR/identity.key`.
   A user may put the port and path in `~/.ssh/config`; do not assume an agent
   loaded the key. Another client device must receive an explicitly authorized
   key before it can enter.
4. **Listener scope.** Bind the tailnet interface, not `0.0.0.0`. Reuse T06's
   address discovery. A host with no tailnet address does not start an SSH
   listener and says so in the log rather than falling back to every interface.
5. **Middleware.** Use what Wish ships rather than reinventing it:
   `recover` outermost, then `logging`, `ratelimiter`, `accesscontrol`, and
   `activeterm` only on the handlers that need a PTY. One-shot exec must work
   without a PTY, so `activeterm` cannot be global.
6. **Lifecycle.** The SSH server starts and stops with the daemon and shares its
   shutdown context. Killing the daemon must not kill sessions, same as always:
   an SSH connection is a client, and clients are disposable.

## Shape

```go
package sshd

type Config struct {
    HostKey       ed25519.PrivateKey // from internal/identity
    AuthorizedKeys string            // path in the state directory
    Addr          string             // tailnet IP and port from T06 and the daemon
}

// Serve runs until ctx is done. Handlers are registered by T16, T17 and T18.
func Serve(ctx context.Context, cfg Config, opts ...ssh.Option) error
```

## Implementation

`internal/sshd.Serve` validates one concrete IP endpoint, exports the existing
Ed25519 identity in OpenSSH private-key format, and locks Wish to public-key
authentication. Extension options can add handlers or subsystems. They cannot
replace the host key, address, or authentication callbacks.

The authentication callback opens `authorized_keys` for every attempt. It
refuses a missing file, a non-regular file, a changed file, a file with no read
bits, a group-writable file, a world-writable file, and a file larger than 1
MiB. Re-reading the file makes key removal effective for the next connection.

The Wish handler runs `recover`, `logging`, `ratelimiter`, and `accesscontrol`
in that order. The temporary `hello` exec command works without a PTY.
`activeterm` remains scoped to the PTY handlers that T17 adds.

Wish 1.4.7 brings the Bubble Tea v1 dependency tree. Mesh keeps its product TUI
on Bubble Tea v2. `github.com/charmbracelet/x/cellbuf` v0.0.15 is the minimum
pin in that legacy tree that compiles with Mesh's `x/ansi` v0.11 line.

`mesh daemon --ssh-port PORT` starts one SSH server for each discovered
Tailnet address. A zero value disables SSH. The Linux and macOS installers pass
`--ssh-port=2222`; port 22 remains available to the system SSH server for
bootstrap and recovery.

## The part that needs care

This is a second authentication surface on every host. The Mesh protocol side is
protected by Tailscale and a 0700 socket directory; this one is protected by a
key file you have to get right.

- An unreadable or missing authorized_keys file means **refuse every connection**,
  not allow every connection. Test that explicitly.
- An authorized_keys file that is group or world writable is a refusal too.
- Do not log public keys of failed attempts at info level; a scan fills the disk.

## Verification

The `internal/sshd` tests use Wish's `testsession`. They cover authorized,
unauthorized, missing, password-only, unreadable, writable, symlinked, and
revoked keys. They also compare the SSH host fingerprint with the Mesh identity
and prove that cancellation closes only the configured listener.

`internal/daemon/ssh_test.go` captures the SSH configurations produced through
the daemon's Tailnet discovery path. It proves that the daemon uses only the
discovered addresses, shares its cancellation context, skips SSH without an
address, and treats an SSH listener failure as a daemon failure.

`integration/ssh_front_door.sh` drives the built daemon with stock OpenSSH. It
uses this exact key selection:

```bash
ssh -p 2222 -o IdentitiesOnly=yes \
  -i "${MESH_STATE_DIR}/identity.key" host.mesh.shaulavo.dev hello
```

The script rejects an unrelated key and a client with no key. It compares the
key from `ssh-keyscan` with `identity.key`, checks address confinement, keeps an
`ssh -N` connection open, and stops the daemon. The SSH connection closes, but
the detached worker remains alive.

Repository gates:

```bash
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/verify.sh
```

Executed on 2026-08-30: formatting, module, and sqlc generation diffs were
clean. The CI-pinned seven-linter run, `go vet ./...`, `go test -race ./...`,
all 18 integration scripts, and the Linux amd64/arm64 and Darwin arm64 builds
passed.

## Out of scope

Sessions (T17), files (T16), tunnels (T18), and writing authorized_keys (T08).
