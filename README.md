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

Run `mesh update` to review one release for your saved fleet. Offline machines
remain pending and retry when they return. The picker shows update reminders.
See [Update Mesh](docs/updates.md) for fleet membership, progress, and retries.

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

To keep every terminal window's shell running, follow
[Use Mesh for every terminal window](docs/terminals.md).

To recover saved directories and previous output after a host crash, follow
[Recover a workspace](docs/recovery.md).

To reopen a saved Codex or Claude conversation after a crash, follow
[Recover an exact conversation](docs/agent-recovery.md).

To stop idle agents and resume them on attach, and to see what each session
costs, follow [Hibernate idle agent sessions](docs/hibernation.md).

To wake a sleeping PC automatically when connecting, follow
[Wake a machine](docs/power.md). The target grants permission with
`mesh wake allow`; Mesh chooses an awake sender on its LAN.

## Remote access

Installed hosts expose Mesh sessions through public-key-only SSH on Tailnet
port 2222. Add the port and your authorized Mesh key to `~/.ssh/config`:

```sshconfig
Host *.mesh.shaulavo.dev
    Port 2222
    IdentityFile ~/.local/state/mesh/identity.key
    IdentitiesOnly yes
```

```bash
ssh pc.mesh.shaulavo.dev          # picker on this host
ssh -t pc.mesh.shaulavo.dev 7K3D  # attach a specific session
ssh pc.mesh.shaulavo.dev ls       # list sessions without a terminal
```

Press `ctrl+]` to return to the picker. Closing SSH leaves the session running.
A phone with Tailscale and an SSH client can use the same commands after you
import an authorized key.

Static and files services are also available as read-only directories over
SFTP and SCP. A service at `/files` uses that same path over SSH:

```bash
mesh serve pc /srv/shared --at /files --files  # directory already on pc
sftp -P 2222 pc.mesh.shaulavo.dev             # browse /files
scp -P 2222 pc.mesh.shaulavo.dev:/files/report.pdf .
scp -O -P 2222 pc.mesh.shaulavo.dev:/files/report.pdf .  # legacy SCP protocol
```

The SFTP root lists the declared directory services. Proxy services are omitted;
uploads and other writes are refused. Service changes take effect without an
SSH restart.

A proxy route can also own the command behind its port. `mesh serve pc --run
'bun run dev' --listen 5173=15173` starts the dev server on the first connection
to `127.0.0.1:5173` and stops it after 15 idle minutes. See
[Serve a dev server on demand](docs/serve-on-demand.md).

To reach an app on your current machine from outside the tailnet, reserve a
hostname with `mesh serve claim vps blog.shaulavo.dev`, then connect a named
SSH reverse forward. See [Reach a local app through the public edge](docs/reverse-tunnels.md)
for the exact identity and SSH command. Disconnecting returns the hostname to 404.

## Development

Run `./scripts/gates.sh` for formatting, vet, golangci-lint, deadcode, ShellCheck,
Ruff, and the bootstrap and terminal dependency contracts. Go analysis targets
Linux/amd64 with CGO disabled so the checked-in baseline is independent of the
machine running it. Python 3 is required for the release tools and baseline checker.

Install golangci-lint v2.13.2, ShellCheck v0.11.0, and Ruff 0.16.9 on your PATH.
CI downloads the ShellCheck and Ruff release archives into its temporary tools
 directory and verifies their SHA-256 checksums. Use the matching OS and architecture
archives from [ShellCheck releases](https://github.com/koalaman/shellcheck/releases/tag/v0.11.0)
and [Ruff releases](https://github.com/astral-sh/ruff/releases/tag/0.16.9).
For example, on Linux/amd64:

```bash
mkdir -p /work/cache/mesh-gates-tools
curl -fsSL https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.x86_64.tar.xz -o /work/cache/mesh-gates-tools/shellcheck.tar.xz
echo '8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198  /work/cache/mesh-gates-tools/shellcheck.tar.xz' | sha256sum -c -
tar -xf /work/cache/mesh-gates-tools/shellcheck.tar.xz -C /work/cache/mesh-gates-tools --strip-components=1
python3 -m pip install --target /work/cache/mesh-gates-tools/python ruff==0.16.9
export PATH="/work/cache/mesh-gates-tools:/work/cache/mesh-gates-tools/python/bin:$PATH"
export PYTHONPATH="/work/cache/mesh-gates-tools/python${PYTHONPATH:+:$PYTHONPATH}"
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
go install github.com/evilmartians/lefthook@v2.1.15
lefthook install
```

Lefthook is a development tool, not a Go module dependency. The pre-commit hook
runs `gates.sh --fast`, which checks formatting, vet, and golangci findings in
staged Go packages. Full gates run in CI. Deadcode runs through
`go run golang.org/x/tools/cmd/deadcode@v0.49.0` without changing `go.mod`.

`.gates/baseline.json` records existing findings with reasons and occurrence counts.
New findings fail. Fixed findings also fail until you run
`./scripts/gates.sh --update-baseline`. That command removes entries or reduces
counts, but refuses all additions without changing the baseline. Any new exception
needs a deliberate manual edit and a reason. `--report-dir DIR` retains raw tool
reports for inspection. `third_party/` is excluded from quality scans. Its SSH
patch contracts remain in the integration suite.

Run `./scripts/verify.sh` for the integration suite. It builds both test binaries
once and runs up to four tests concurrently, limited by available CPUs. Set
`MESH_INTEGRATION_JOBS` to choose a different concurrency limit. See the
[implementation status](docs/plan/02-status.md) and [task briefs](docs/tasks/)
for the design and build order.
