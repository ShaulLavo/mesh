# Files, ports and environment

Where Mesh keeps its state, which ports an installed daemon opens, and which environment variables it reads or sets. Checked against a v0.1.251 build on macOS.

## State directory

`MESH_STATE_DIR` if set. Otherwise `$XDG_STATE_HOME/mesh`. Otherwise `~/.local/state/mesh`. Mesh creates it with mode `0700`. Keep it short: Unix socket paths are limited to about 100 bytes.

| Path | Holds |
|---|---|
| `identity.key` | This device's Ed25519 Mesh key. Never replace it on a host you adopted. |
| `daemon.sock`, `daemon.lock` | The daemon's Unix control socket and its lock. |
| `mesh.db` (and `-wal`, `-shm`) | The SQLite session catalog the daemon keeps. |
| `machine-name.json` | The machine's declared name and revision. See [Machine names](machine-names.md). |
| `s/<ID>/` | One directory per session. |
| `s/<ID>/sock` | The worker's Unix socket. Present while the worker runs. |
| `s/<ID>/meta.json` | State, exit code and kernel boot ID, written by the worker. |
| `s/<ID>/recovery.json` | The worker's recovery checkpoint. |
| `s/<ID>/worker.log` | The worker's log. |
| `terminals/`, `updates/`, `wake/` | Terminal-window bindings, update operations, wake permissions. |

## Config directory

`MESH_CONFIG_DIR` if set. Otherwise `$XDG_CONFIG_HOME/mesh`. Otherwise `~/.config/mesh`.

| Path | Holds |
|---|---|
| `hosts.json` | The address book that `mesh add` writes, and the saved dashboard theme. |
| `domains.json` | The deployment-domain policy. See [Deployment domains](deployment-domains.md). |

## Ports

An installed daemon listens as follows. The daemon's own defaults are off (port `0`). The installers set these values.

| Port | Protocol | Flag |
|---|---|---|
| 7337, path `/mesh` | WebSocket with TLS 1.3 inside, between Mesh clients and hosts | `--tailnet-port`, `--websocket-path` |
| 2222 | SSH, public key only | `--ssh-port` |

SSH and WebSocket listen on the host's discovered Tailnet addresses.

## Installed service

| Platform | Service file | Logs |
|---|---|---|
| Linux | `~/.config/systemd/user/mesh.service` (a user service with lingering enabled) | the user journal |
| macOS | `~/Library/LaunchAgents/dev.shaulavo.mesh.plist`, label `dev.shaulavo.mesh` | `~/.local/state/mesh/daemon.log` and `daemon.err.log` |

The installers put the binary at `~/.local/bin/mesh` unless `MESH_BIN_DIR` says otherwise.

## Environment variables you set

| Variable | Effect |
|---|---|
| `MESH_STATE_DIR` | Overrides the state directory. |
| `MESH_CONFIG_DIR` | Overrides the config directory. |
| `MESH_PRIVACY` | Same as `--privacy`. See [Privacy mode](../guides/privacy.md). |
| `MESH_TERMINAL_ID` | Names the terminal tab so `mesh back` can remember its session. |

Installer variables for `scripts/install.sh`:

| Variable | Effect |
|---|---|
| `MESH_INSTALL_YES=1` | Skip the confirmation prompt, which needs gum v2. |
| `MESH_VERSION` | Install this release tag instead of the latest. |
| `MESH_BIN_DIR` | Absolute directory for the binary. |
| `MESH_SKIP_SERVICE=1` | Install the binary and no service. |

## Environment variables Mesh sets

Every session's process gets these.

| Variable | Value |
|---|---|
| `MESH_SESSION_ID` | The session ID. |
| `MESH_HOST_ID` | The host's ID. |
| `MESH_DEPTH` | How deeply this client nests inside other Mesh sessions. |
| `MESH_STATE_DIR`, `MESH_CONFIG_DIR` | The directories the session's worker uses. |
