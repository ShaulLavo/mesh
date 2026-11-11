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

The follow-up run at `0967b9da` passed mapped native executable controls on all
three platforms, including Linux ARM64. The control execs `/bin/sleep`, checks
its mapped inode and digest through procfs or native `lsof`, and checks the
manager PID again. Native installed-pair startup and cleanup also ran on all
three platforms. The first stream-registration attempt used a URL where the
proxy control requires a port, so it failed before restart. The fixture now
supplies the correct port. All task processes, units and scratch were confirmed
removed in that failed run's artifacts.

The next run reached restart and retained the worker, shell and listener, but a
local Unix attachment ends on daemon disconnect by design. The fixture now
uses the published CLI's real authenticated WebSocket transport, a separate
test-owned client identity and a pinned daemon endpoint. A fresh nonce printed
by the original session-owned listener proves that the same client reconnected
before new input is sent. A local supplemental test with actual published
v0.1.249 confirmed that path and unchanged identities. Hosted native acceptance
still requires its own passing result. Teardown also handles an already exited
client without writing to its closed PTY.

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
