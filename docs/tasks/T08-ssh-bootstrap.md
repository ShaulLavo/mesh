# T08 — `mesh add shaul@pc`

**Status:** complete · **Blocked by:** T04, T06 · **Owns:** `internal/bootstrap/`,
`scripts/install/`

## Goal

One command turns an SSH-reachable machine into a Mesh host. SSH is not part of
the normal connection path after bootstrap.

Bootstrap detects the operating system and architecture, selects a matching
binary, installs a user service, starts the daemon, discovers the Mesh and
Tailscale identities, verifies the direct WebSocket, and gives the CLI a host
record to save.

## Implementation

`bootstrap.Run` accepts the SSH target, the local state directory, optional
binary-release settings, SSH prompt callbacks, and an optional pinned Mesh
identity. It returns the stable host ID, the Mesh identity, the Tailscale name
and addresses, the verified daemon endpoint, the detected platform, and whether
the remote installation was already current.

The SSH client reads the standard SSH agent and `~/.ssh/id_*` keys. It checks
`known_hosts`. The CLI supplies callbacks for a password, an encrypted-key
passphrase, and confirmation of an unknown host key. A confirmed host key is
appended to `known_hosts`; a changed known key is always refused.

Binary selection uses this order:

1. Use `Options.BinaryPath` when the caller supplied one.
2. Use the running executable when its format matches the remote platform.
3. Use a matching raw binary or release archive next to the executable.
4. Download a release archive from GitHub over HTTPS and verify its SHA-256
   entry in `checksums.txt` before extraction.

T10 must publish `mesh_<os>_<arch>.tar.gz` and `checksums.txt`. Each
archive contains one regular file named `mesh`. Release builds set
`github.com/shaul/mesh/internal/bootstrap.releaseVersion` with the Go linker
`-X` flag, so a Mac fetches the matching Linux or Pi release. Development
builds use the latest release when no sibling artifact matches.

`scripts/install/linux.sh` installs `~/.local/bin/mesh`, writes a
systemd user unit, enables the unit, and starts it. The script refuses a Linux
host without systemd or user lingering. `scripts/install/darwin.sh` writes a
LaunchAgent and loads it into the available user domain. Both scripts install
the adopter's Mesh identity in `authorized_keys`. They compare file contents
before replacement and report `unchanged` on a converged rerun.

After installation, bootstrap reads `tailscale status --json`, checks the
remote clock, and tries every reported Tailscale address until the verification
deadline. Verification sends `host.info` over the direct WebSocket. It
checks that the daemon ID is a valid Ed25519 identity and matches any identity
the CLI already pinned.

T07 owns alias policy and `hosts.json`. The command adapter pins a saved
identity before bootstrap and atomically saves the verified host record only
after `bootstrap.Run` succeeds. Operation metadata stays outside the durable
host record so a converged rerun can report `already configured` accurately.

## Dependencies

`golang.org/x/crypto/ssh` provides the SSH protocol, agent access,
`known_hosts` verification, and key parsing. The standard library has no SSH
client.

Huh v2 renders the three interactive prompts in the T07 command adapter. Prompt
callbacks keep Huh out of the bootstrap package, so tests and non-interactive
callers can provide fixed decisions.

## Failure diagnostics

Every common failure has a stable `DiagnosticCode`, a cause, and a recovery
command.

| Failure | Code | Recovery |
|---|---|---|
| SSH is unreachable | `ssh_connect` | Run `ssh user@host` and check the host, port, and route. |
| SSH rejects every credential | `ssh_auth` | Load a working key into the SSH agent or enter the SSH password. |
| The host key is unknown or changed | `ssh_host_key` | Verify the fingerprint. Confirm a new key or fix `known_hosts`; never accept a changed key automatically. |
| No matching release binary exists | `wrong_arch` | Publish or place the reported `os/arch` artifact and retry. |
| Linux has no user systemd | `no_systemd` | Install user-systemd support and run `systemctl --user status`. |
| The user stops when SSH exits | `no_user_lingering` | On the remote host, run `sudo loginctl enable-linger $USER`. |
| Tailscale is missing or stopped | `tailscale_unavailable` | Install or start Tailscale on the remote host. |
| Tailscale is logged out | `tailscale_logged_out` | Run `tailscale up` on the remote host. |
| The daemon port cannot be reached | `port_blocked` | Run `tailscale ping`, then allow TCP 7337 in the ACL and host firewall. |
| The clocks differ by more than five minutes | `clock_skew` | Enable network time with `timedatectl set-ntp true` or `sudo sntp -sS time.apple.com`. |
| The daemon reports a changed identity | `identity_verification` | Inspect the remote state. Do not replace `identity.key` to silence the error. |

The focused tests cover every listed code. `TestConnectSSHNamesAuthenticationFailure`
uses a real in-process SSH server. `TestCheckBinaryPlatformNamesWrongArchitecture`,
`TestInstallRemoteMapsInstallerFailures`,
`TestDiscoverTailnetNamesLoggedOutState`,
`TestVerifyWebSocketNamesBlockedPort`, and
`TestCheckRemoteClockNamesSkew` cover the remaining named boundaries.

## Verification

`integration/bootstrap_installers.sh` runs both service installers against
isolated homes. It proves the first run configures the service, the second run
reports `unchanged`, and the authorized key is present once with mode 0600.
It also executes the `no_systemd` and `no_user_lingering` failure
paths.

The development machine cannot run a real user-systemd VM or a macOS launchd
domain. Reproduce those service-manager boundaries before a release:

1. On a Linux VM, run `sudo loginctl enable-linger $USER`, then run
   `mesh add user@vm` from a tailnet peer.
2. Disconnect SSH. Confirm
   `ssh user@vm systemctl --user is-active mesh.service` prints `active`.
3. Run `mesh add user@vm` again. Confirm that it reports the existing
   installation and preserves the host ID.
4. On macOS, run `mesh add user@mac`. Confirm
   `launchctl print gui/$(id -u)/dev.shaulavo.mesh` or the matching
   `user` domain shows the loaded service.
5. Log out and back in. Run `mesh add user@mac` again and confirm the
   direct endpoint still verifies.

The final repository checks are:

```bash
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/verify.sh
```

All four checks pass on the Linux development host. `scripts/verify.sh` runs
all 12 integration scripts, including the isolated systemd and launchd
installer harness. The real user-systemd VM and macOS launchd checks above
remain release-time manual tests because those service managers are not
available in this environment.
