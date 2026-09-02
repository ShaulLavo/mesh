# mesh

Mesh keeps terminal sessions running on their host while clients disconnect
and reattach. It connects your machines over Tailscale.

[**How it is used**](https://excalidraw.com/#json=KGCNqa1YJB7z8KCBx5_RE,ZNE_9E61xji2A2eFRbM2mg)
— start a session, walk away, pick it up somewhere else. The source lives at
[`docs/diagrams/usage.excalidraw`](docs/diagrams/usage.excalidraw), so the link
can be regenerated if it ever goes stale.

## Install

macOS:

```bash
brew tap ShaulLavo/mesh https://github.com/ShaulLavo/mesh
brew install --cask ShaulLavo/mesh/mesh
```

Releases are ad-hoc signed by the Go linker rather than with an Apple Developer
ID, so macOS quarantines the binary and Gatekeeper offers only to move it to
the trash. The cask clears that attribute on install. Homebrew removed
`--no-quarantine`, so a binary downloaded by hand needs it cleared by hand:

```bash
xattr -d com.apple.quarantine ./mesh
```

Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/ShaulLavo/mesh/main/scripts/install.sh | sh
```

The installer verifies the release checksum before it publishes the binary.

## Run it

```bash
go build -o mesh ./cmd/mesh

./mesh local          # start a session
./mesh ls             # list local sessions
./mesh local -r       # reattach to the latest session
./mesh add user@host  # install Mesh on another machine
```

If the remote host lacks Tailscale, `mesh add` shows the package-manager
commands and asks before it runs them. For an unattended adoption, pass
`--yes --tailscale-auth-key-file ./key`.

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
