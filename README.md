# mesh

Mesh keeps terminal sessions running on their host while clients disconnect
and reattach. It connects your machines over Tailscale.

## Run it

```bash
go build -o mesh ./cmd/mesh

./mesh local          # start a session
./mesh ls             # list local sessions
./mesh local -r       # reattach to the latest session
./mesh add user@host  # install Mesh on another machine
```

Press `ctrl+]` to detach without stopping the command.

## Remote access

Installed hosts expose public-key-only SSH on Tailnet port 2222. The current
front door confirms authentication and exits:

```bash
ssh -p 2222 -o IdentitiesOnly=yes \
  -i ~/.local/state/mesh/identity.key host.mesh.shaulavo.dev hello
```

SFTP, remote SSH sessions, and named reverse tunnels are the next tasks.

## Development

Run `./scripts/verify.sh` for the integration suite. See the
[implementation status](docs/plan/02-status.md) and [task briefs](docs/tasks/)
for the design and build order.
