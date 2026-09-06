# Reach a local app through the public edge

Use a reverse tunnel to reach an app running on your current machine from
outside the tailnet. The app stays on that machine. Disconnecting SSH removes
its public route.

The examples assume that `vps` is an adopted Mesh host with the
[public edge configured](tasks/T13-public-edge.md). Your local Mesh identity
must be in the edge's managed `authorized_keys`. Both machines use Tailscale
for the SSH connection.

## Claim a hostname

Reserve the exact hostname you want to use:

```bash
mesh serve claim vps blog.shaulavo.dev
```

Read the public confirmation and accept it. Use `--yes` to skip the prompt.
Cancelling creates no reservation. A hostname must contain exactly one label
below `shaulavo.dev`; short names and wildcards are refused.

A claim reserves the whole hostname, including every path. It cannot overlap
an existing public service. The claim alone returns HTTP 404.

## Connect your app

Start your app on local port 3000, then run:

```bash
mesh_identity="${MESH_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/mesh}/identity.key"
ssh -N -o ExitOnForwardFailure=yes -o IdentitiesOnly=yes -i "$mesh_identity" \
	-p 2222 \
	-R blog.shaulavo.dev:80:localhost:3000 vps.mesh.shaulavo.dev
```

Open `https://blog.shaulavo.dev`. The hostname in `-R` must exactly match the
claim. Port `80` identifies the existing HTTP front door; SSH does not open
a listener at that hostname and port. Change `3000` to your app's local port.

The command uses the same Mesh identity that signed the claim. Your default SSH
key might identify someone else. `ExitOnForwardFailure` makes a refused forward
exit instead of leaving an idle connection.

Press Ctrl+C to disconnect. The hostname returns 404 and its claim remains
reserved. Run the same SSH command to reconnect. An edge restart also leaves
every claim inactive. A silent connection failure triggers keepalive cleanup
within 60 seconds.

## Release the hostname

Disconnect its active SSH forward, then release the reservation:

```bash
mesh unserve blog.shaulavo.dev --host vps
```

Release requires the claiming identity's signature. Removing that identity
from the edge's `authorized_keys` blocks new claims and activations, but the
owner can still release an inactive claim.

If you lose the claiming key, run this on the edge itself:

```bash
mesh unserve blog.shaulavo.dev --local-edge
```

Recovery uses the local daemon socket and also stops an active forward. The
tailnet control listener refuses this operation. The edge retains the retired
owner's replay protection after recovery.
