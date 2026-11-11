# Installed catch-up fixture

Unit 8 is in progress. The native prerequisites and published-pair fixture are
implemented. Adoption, signed automatic hops, rollback and ordered-host acceptance
are still required before this is the complete installed publication gate.

## Native manager positive controls

[Hosted run 43113902836](https://github.com/ShaulLavo/mesh/actions/runs/43113902836)
passed at source `d5fe07af` on both `ubuntu-24.04` and `macos-15`. Linux used the
actual systemd user manager. macOS used the actual launchd `gui/501` domain.
Each manager executed a test-owned program, reported the same PID as the child,
and removed the service and process. The normal and exception-after-launch
controls passed. No VM fallback, credential, OS configuration or live fleet
change was needed.

Later fixture code also checks the mapped executable's inode and digest with
procfs or native `lsof`, then checks the manager PID again. That refinement
requires its own hosted result. Linux ARM64 is a separate native matrix entry.

`installed_paths.py` resolves the physical root, checks the worst daemon,
temporary-daemon and retained-worker socket paths for every case, and refuses
an unmounted `/work` or `/data` destination. Tests bind every generated address,
exercise the exact Linux and Darwin limits, count filesystem bytes and reject
an overlong symlink destination. Service cleanup runs before scratch removal.
Unconfirmed cleanup retains the root and fails the proof.

## Actual published pairs

`fleet-baselines.json` records the read-only November 11 inventory. The PC pair
begins with its helper stopped. The first published updater remains `null`
until a real release containing the updater has been published. A current
source build with an old version label cannot supply that baseline.

`fleet_baselines.py` reuses the shared system-trusted public HTTPS downloader,
archive verifier and executable verifier. It checks the recorded image hashes
where the inventory supplied them, rechecks cache bytes and executes the native
published program's `version --json`. Local Linux acquisition verified the
actual PC 249/196 and VPS 255/196 pairs. Downloads, archives and executables were
removed after inspection.

The native installed fixture refuses existing Mesh units before registering
anything. It uses the production `PrepareHelper` API for the historical helper
image, actual systemd units or launchd plists, one attached PTY client, and an
HTTP listener owned by that session. Its initial restart control checks the
original worker, shell and listener, reconnects the service stream, bounds the
route gap and checks the running helper image. It makes no adoption or automatic
acceptance claim. The real fleet is not involved.

## Reusable job contract

The caller uses `.github/workflows/installed-catchup.yml` with `contents: read`.
It takes no inputs or secrets. Its current contract is native prerequisite and
published-pair restart validation, not the complete Unit 8 gate. Unit 3 owns
`ci.yml`, `release.yml` and the eventual aggregate publication requirement.

The workflow runs its native positive control before Go setup, builds or release
downloads. Each installation has bounded operations and owns its service units,
helper images, session processes and socket root. It preserves small JSON and
log evidence for seven days. Test roots and binaries are removed after their
consumers stop. Local reproducer and retained hosted evidence are under
`/work/reports/mesh-auto-update/unit8-installed-catchup/`; the parent owns their
retirement after the lane's final review and merge.
