# Reach sessions over SSH

The examples use `new.example` from the configured
[deployment domains](deployment-domains.md). Replace it with your domain.

Installed hosts expose Mesh sessions through public-key-only SSH on Tailnet
port 2222. Add the port and your authorized Mesh key to `~/.ssh/config`:

```sshconfig
Host *.mesh.new.example
    Port 2222
    IdentityFile ~/.local/state/mesh/identity.key
    IdentitiesOnly yes
```

```bash
ssh pc.mesh.new.example          # picker on this host
ssh -t pc.mesh.new.example 7K3D  # attach a specific session
ssh pc.mesh.new.example ls       # list sessions without a terminal
```

Press `ctrl+]` to return to the picker. Closing SSH leaves the session running.
A phone with Tailscale and an SSH client can use the same commands after you
import an authorized key.

Static and files services are also available as read-only directories over
SFTP and SCP. A service at `/files` uses that same path over SSH:

```bash
mesh serve pc /srv/shared --at /files --files  # directory already on pc
sftp -P 2222 pc.mesh.new.example             # browse /files
scp -P 2222 pc.mesh.new.example:/files/report.pdf .
scp -O -P 2222 pc.mesh.new.example:/files/report.pdf .  # legacy SCP protocol
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

To reach an app on your current machine from outside the tailnet, reserve a
hostname with `mesh serve claim vps blog.new.example`, then connect a named
SSH reverse forward. See [Reach a local app through the public edge](reverse-tunnels.md)
for the exact identity and SSH command. Disconnecting returns the hostname to 404.
