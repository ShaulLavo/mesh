# Reach sessions over SSH

Installed hosts expose Mesh sessions through public-key-only SSH on port 2222. The
listener binds only the host's Tailscale addresses, so the client must be on the same
tailnet.

## Pick a hostname

Any name or address your client already resolves to the host works. With
Tailscale MagicDNS on, the host's tailnet name works with no Mesh setup. Find it
on the host with `tailscale status --self`. The short name, the full name and the
Tailscale IPv4 address each reached a running host:

```text
pc                        # short MagicDNS name
pc.tailnet-name.ts.net    # full MagicDNS name
100.x.y.z                 # Tailscale IPv4 address
```

Names of the form `pc.mesh.<domain>` exist only when the deployment has
[deployment domains](../reference/deployment-domains.md) configured and a
certificate renewer publishing DNS for them. A first-time install has neither,
so use a tailnet name or address. The examples below use the short name `pc`.

## Use the device key

Add the port and your Mesh identity key to `~/.ssh/config`. Mesh stores this
device's Ed25519 key at `~/.local/state/mesh/identity.key` in OpenSSH format, so
a stock `ssh` can read it. `mesh add` approved that key on the host, so it is
already authorized. Another SSH key, such as `~/.ssh/id_ed25519`, is refused with
`Permission denied (publickey)` until you approve it. `IdentitiesOnly yes` stops
`ssh` from offering other keys first.

```sshconfig
Host pc
    Port 2222
    IdentityFile ~/.local/state/mesh/identity.key
    IdentitiesOnly yes
```

```bash
ssh pc          # picker on this host
ssh -t pc 7K3D  # attach a specific session
ssh pc ls       # list sessions without a terminal
```

The host's SSH host key is the same Mesh identity, exported in OpenSSH format
([ADR 005](../adr/005-mesh-keys-authorize-controls.md)). Mesh has no second keypair.

Press `ctrl+]` to return to the picker. Closing SSH leaves the session running.
A phone with Tailscale and an SSH client can use the same commands after you
approve its key. Generate an Ed25519 key in the phone's SSH client, then run this
on the host with its public key:

```bash
mesh device approve --public-key -- 'ssh-ed25519 AAAA... phone'
```

An approved device controls the host's OS account. See
[Approve a device](approve-devices.md) and [Security notes](../concepts/device-access.md#security-notes).

Static and files services are also available as read-only directories over
SFTP and SCP. A service at `/files` uses that same path over SSH:

```bash
mesh serve pc /srv/shared --at /files --files  # directory already on pc
sftp -P 2222 pc             # browse /files
scp -P 2222 pc:/files/report.pdf .
scp -O -P 2222 pc:/files/report.pdf .  # legacy SCP protocol
```

The SFTP root lists the declared directory services. Proxy services are omitted;
uploads and other writes are refused. Service changes take effect without an
SSH restart.

A proxy route can also own the command behind its port. `mesh serve pc --run
'bun run dev' --listen 5173=15173` starts the dev server on the first connection
to `127.0.0.1:5173` and stops it after 15 idle minutes. See
[Serve a dev server on demand](serve-on-demand.md).

Service lists and the dashboard can show a descriptive name alongside a route.
Set it when publishing with `--label 'Fregat dev'`, or rename an existing entry
with `mesh serve label :5173 'Fregat dev' --host pc`. The label is stored on the
serving host and survives later publications. Changing it keeps the route, ports,
and running process.

Mesh services and apps are Tailnet-only. Connect the viewing device to Tailscale
and use the private HTTPS URL. Public serving and VPS-edge reverse tunnels were
removed on 2026-10-08; public hosting belongs to Brine on `shaulavo.dev`.
