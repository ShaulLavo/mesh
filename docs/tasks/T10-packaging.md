# T10 — Packaging and CI

**Status:** not started · **Blocked by:** T07 · **Owns:** `.github/`, `.goreleaser.yaml`,
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
