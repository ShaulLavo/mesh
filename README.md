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
machine running it. The baseline checker uses only the Go standard library.
Python 3 is needed for the existing release tools and the CI-only planted-violation proof.

From the repository root, install the pinned tools and hooks once:

```bash
mise install
lefthook install
```

`mise.toml` pins Go 1.27.0, golangci-lint 2.13.2, ShellCheck 0.11.0, Lefthook
2.1.15, and Ruff 0.16.9. Use `mise exec -- lefthook install` if your shell does
not activate mise. CI verifies the ShellCheck, Ruff, and Lefthook release checksums.
Lefthook is a development tool, not a Go module dependency.

Pre-commit runs `gates.sh --fast`. It checks gofmt on staged Go files, vet and
golangci findings in their packages, and ShellCheck on staged shell files.
It inspects an isolated snapshot of the Git index, so unstaged changes cannot
hide staged violations or block an unrelated commit. Only Go, golangci-lint,
ShellCheck, and Lefthook are needed for this hook. Missing or mismatched tools
fail with an install hint. Full scans alone enforce removal of stale baseline
entries because partial package graphs can omit cross-package findings.

Lefthook installs hooks in the common Git directory. The same installation
covers linked worktrees. `assert_lefthook_installed` makes a missing Lefthook
executable fail rather than silently skip checks. Each hook resolves its config and `scripts/gates.sh`
from the worktree where Git runs it. Pre-push runs the full gates command and
also requires Ruff. Full gates run in CI. Deadcode runs through
`go run golang.org/x/tools/cmd/deadcode@v0.49.0` without changing `go.mod`.

`.gates/baseline.json` records existing findings with reasons and occurrence counts.
Go lint keys use file, rule, normalized diagnostic, and enclosing Go function, not a
source-line fingerprint. Diagnostic line numbers, complexity scores, and contextcheck
SSA closure ordinals are omitted; callee paths and literal digits remain significant.
Growth of an already-baselined complexity score is not ratcheted; new findings and
per-key finding-count growth still fail. Goconst uses only the literal within its file,
so a representative occurrence or reported count can change without churning the key.
ShellCheck and Ruff retain source-statement keys, preventing a fixed exception from
covering an unrelated replacement warning. Goconst, gocognit, and dupl exclude
`_test.go`; all security linters still check tests. Dupl's exclusion filters report
location, not analyzer input, so a production-side report of a test/production clone
still fails unless baselined.
New findings fail. Fixed findings also fail until you run
`./scripts/gates.sh --update-baseline`. That command removes entries or reduces
counts, but refuses all additions without changing the baseline. Any new exception
needs a deliberate manual edit and a reason. `--report-dir DIR` retains raw tool
reports for inspection. `third_party/` is excluded from quality scans. Its SSH
patch contracts remain in the integration suite.

Run `./scripts/verify.sh` for the integration suite. It builds both test binaries
once and runs up to four tests concurrently, limited by available CPUs. Set
`MESH_INTEGRATION_JOBS` to choose a different concurrency limit. Each script gets
a scratch HOME and an allow-listed environment. The verifier resolves Go and
Python before replacing HOME. It preserves user-bus discovery so Linux scope
checks cannot silently skip coverage, plus `MESH_TEST_ZSH` and `MESH_SHORT_TMP`.
The initial binary builds keep effective Go module policy, proxy settings, CA
trust, and an absolute netrc path for dependency downloads. Those build-only
settings do not reach tests. Fixtures set any provider or proxy settings they
need. Standalone agent scripts also scrub inherited settings and pin Python
through their shared fixture. Use the verifier to isolate the full shell-script
environment. See the
[implementation status](docs/plan/02-status.md) and [task briefs](docs/tasks/)
for the design and build order.
