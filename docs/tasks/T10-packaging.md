# T10 — Packaging and CI

**Status:** complete · **Blocked by:** T07 · **Owns:** `.github/`, `.goreleaser.yaml`,
`scripts/install/`

## Goal

Ship it: systemd unit, launchd plist, GoReleaser, GitHub Actions, Homebrew Cask,
a Gum-powered installer, VHS demos.

CI must run `go test -race ./...`, the integration scripts, `govulncheck`, and
golangci-lint v2. Cross-compile targets: linux/amd64, linux/arm64 (the Pi),
darwin/arm64.

## Acceptance

- A tagged release produces working binaries for all three targets.
- The systemd unit survives a reboot and restarts the daemon without touching
  session state — sessions come back `interrupted`, per invariant 5, and the
  daemon reports that rather than crashing on stale directories.
- The integration scripts run in CI, not just locally.

## Release contract

GoReleaser v2.18.0 builds only `linux/amd64`, `linux/arm64`, and
`darwin/arm64`. A release has these download artifacts:

```text
mesh_linux_amd64.tar.gz
mesh_linux_arm64.tar.gz
mesh_darwin_arm64.tar.gz
checksums.txt
```

Each archive contains one regular file named `mesh`. Release builds set
`github.com/shaul/mesh/internal/bootstrap.releaseVersion` to the exact Git tag.
This matches T08's release lookup and keeps the leading `v` in the download URL.
GoReleaser also writes `Casks/mesh.rb` to the Mesh repository. Use it as a
custom Homebrew tap:

```bash
brew tap ShaulLavo/mesh https://github.com/ShaulLavo/mesh
brew install --cask ShaulLavo/mesh/mesh
```

The release workflow first runs race and integration tests, builds a snapshot
candidate, and passes the retained packaging checker against that candidate.
Only then does it publish the tagged release and Cask.

## Installer and services

The interactive installer requires Gum v2. It detects the local platform,
downloads the matching release archive and `checksums.txt`, verifies SHA-256,
checks the archive shape, and publishes the binary atomically:

```bash
curl -fsSL https://raw.githubusercontent.com/ShaulLavo/mesh/main/scripts/install.sh | sh
```

Service assets have one canonical source in `scripts/install/assets/`.
T08 embeds and renders those files before it sends them over SSH. The standalone
installer downloads the same assets from the resolved release tag and verifies
them against that tag's service checksum manifest. This prevents a newer
installer on `master` from silently pairing with older release assets.

Before publishing a binary or service file, each installer creates
`$HOME/.local/state/mesh/activation.pending`. It clears the marker only after
the service manager confirms that the daemon is active. If activation is
interrupted, the next run reloads and restarts the service even when the files
already match. On macOS, the installer also reconciles the label across both
the `user/<uid>` and `gui/<uid>` launchd domains, so a later graphical login
cannot leave two daemon jobs loaded.

The systemd user unit uses `KillMode=process`. The launchd agent uses
`AbandonProcessGroup=true`. Restarting or upgrading the daemon therefore stops
only the daemon process; detached session workers and their state directories
remain. An unchanged installer rerun does not restart the daemon. A machine
reboot still stops workers, and daemon reconciliation reports those sessions as
`interrupted`.

## CI and retained proof

CI pins checkout v7.0.1, setup-go v7.0.0, GoReleaser Action v7.2.3, and
golangci-lint Action v9.3.0 to immutable commit SHAs with version comments. It
also pins golangci-lint v2.13.2 and govulncheck v1.7.0. The release validation
job has read-only repository access; only its dependent publication job can
write release contents. CI runs `go test -race ./...`, every
`integration/*.sh`, `go vet`, golangci-lint v2, govulncheck, a GoReleaser
snapshot, and the packaging contract.

The lint configuration keeps `default: none` so the policy is explicit, but its
allowlist covers correctness and public-boundary failures: `bodyclose`,
`errcheck`, `gosec`, `govet`, `ineffassign`, `staticcheck`, and `unused`.
Deprecation (`SA1019`), nil-context (`SA1012`), and empty critical-section
(`SA2001`) checks remain enabled globally. Intentional cases use a justified
line-level suppression instead of weakening the repository policy.

`scripts/check-packaging.sh` is the retained contract checker. Without release
credentials it:

- cross-builds all three supported targets and reads their Go build metadata;
- checks the GoReleaser archive, linker, Cask, and URL configuration;
- checks the tagged workflow validates a candidate before publishing;
- checks service restart and boot enablement semantics;
- checks T08 embeds the canonical service assets;
- checks the lint allowlist includes the boundary linters and does not disable
  the restored Staticcheck rules;
- checks all demo commands are real and exit successfully; and
- when given `dist/`, verifies the three exact archive names, executable
  one-file archive contents, target metadata, every checksum, and that the
  generated Cask installs `mesh` with the Darwin archive's exact checksum.

`integration/packaging_contract.sh` runs that checker and installs a synthetic
release into an isolated home. It proves a rerun converges without a daemon
restart, an activation failure converges on the next run, and a user-domain
launchd job migrates to the GUI domain without leaving a duplicate. It also
proves that a custom binary directory is reflected in the unit without changing
its permissions, hostile home paths cannot alter the launchd plist, and corrupt
binary or service artifacts are rejected before any file is published. The
existing `integration/reboot_simulation.sh` protects the reboot-to-`interrupted`
contract.

The VHS v0.11.0 tapes in `docs/demos/` are deterministic and rerunnable. Render
both with an exact local VHS version or the pinned container image:

```bash
./scripts/render-demos.sh
```

## Dependency rationale

T10 follows T07 because release, Cask, installer, and demo contracts require the
final Cobra/Fang command surface. It was rebased after T09 so CI locks and tests
the final Charm dependency graph. T08 intentionally landed first: its release
consumer defines the archive names, checksums, and linker version that T10 now
produces. The shared service assets preserve that boundary instead of creating a
second packaging-only service definition.

## Verification

Run the full local gate:

```bash
gofmt -w $(git ls-files '*.go')
go mod tidy
git diff --check
go vet ./...
go test -race ./...
./scripts/verify.sh
./scripts/check-packaging.sh dist
```

GoReleaser v2.18.0 must pass `goreleaser check` and
`goreleaser release --snapshot --clean` before the final checker command.

The automated harness cannot create a real macOS launchd domain or reboot a
user-systemd VM. Reproduce those boundaries before a release:

1. Install Mesh on a Linux VM with user lingering enabled. Start a detached
   session, rerun the installer with a changed daemon binary, and confirm the
   worker PID and session output survive the daemon PID change.
2. Reboot the VM. Confirm the service starts automatically and the old session
   appears as `interrupted` rather than crashing daemon startup.
3. On macOS, install Mesh, start a detached session, and rerun the installer.
   Confirm `launchctl print gui/$(id -u)/dev.shaulavo.mesh` is loaded and the
   worker survives the daemon restart.
