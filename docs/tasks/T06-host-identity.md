# T06 — Host identity and Tailscale discovery

**Status:** not started · **Blocked by:** nothing · **Owns:** `internal/identity/`,
`internal/tailnet/`

## Goal

Every host has a stable Mesh identity independent of its IP, hostname, or
Tailscale name, and Mesh can find the address to reach it on.

## Design

**Identity.** Generate an ed25519 keypair on first run, stored `0600` in the state
dir. The public key is the host ID. It must survive reinstalls of Tailscale, IP
changes, and renames — that is the entire point of not keying off hostname.

```go
package identity

type Host struct {
    ID        string // stable, derived from the public key
    PublicKey ed25519.PublicKey
}

func LoadOrCreate(stateDir string) (Host, ed25519.PrivateKey, error)
```

**Discovery.** Ask the local Tailscale for this machine's name and addresses.
Prefer `tailscale status --json` over the Go client library for v0: fewer
dependencies, and it fails in ways we can explain to the user.

```go
package tailnet

type Peer struct {
    Name    string   // MagicDNS name
    Addrs   []string
    Online  bool
}

func Self(ctx context.Context) (Peer, error)
func Peers(ctx context.Context) ([]Peer, error)
```

Both must fail *informatively* when Tailscale is not installed, not running, or
logged out — those are the three states a user will actually hit, and "connection
refused" is not an acceptable answer for any of them.

## Acceptance

- `LoadOrCreate` is idempotent, creates `0600` files, and round-trips.
- `tailnet` parses real `tailscale status --json` output (check a fixture in).
- Explicit tests for the three failure modes above, asserting the error text names
  the problem and the fix.
- Works on a machine with no Tailscale at all: `mesh local` must not degrade.

## Notes

Tailscale is **not installed** on the dev desktop as of 2026-08-29. Fixture-driven
tests are therefore mandatory, not optional.

## Out of scope

Trust decisions and key pinning between hosts (that lands with `mesh add`, T08).
Record the identity; do not yet decide what it authorizes.
