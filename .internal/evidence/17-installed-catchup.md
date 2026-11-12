# Installed catch-up fixture

Unit 8 is in progress. The real-manager adoption cases and signed cohort/format
fixtures are implemented. The complete publication gate still needs native
cohort acceptance, ordered hosts, interruption coverage and real draft assets.
The product default remains manual. `firstPublishedUpdater` remains `null` until
an actual updater release exists. Versions 9001 through 9003 are supplemental
source builds with test trust roots and are never published.

## Native controls and adoption

[Run 43121166127](https://github.com/ShaulLavo/mesh/actions/runs/43121166127)
passed native service-manager, mapped executable and original published-pair
restart controls on Linux AMD64, Linux ARM64 and macOS ARM64. Linux uses the
actual systemd user manager. macOS uses the actual launchd GUI domain. Each
control records pre-existing resources before registering anything and checks
that its owned service and process are absent after normal and exceptional exit.

[Run 43145373056](https://github.com/ShaulLavo/mesh/actions/runs/43145373056)
passed all four real-manager adoption cases on ARM64 Linux and macOS. The cases
cover a genuine committed prior transaction, successful side adoption, failed
daemon activation rollback, failed helper promotion and initial adoption without
a journal. The original authenticated WebSocket client, worker, shell and service
listener survive. Fresh terminal token I/O and a reconnected service stream
verify their continued operation. The first candidate start sees the explicit
automatic policy. Identity, grants, daemon definition, configuration and prior
installation settings remain unchanged.

On AMD64 that run passed PC adoption and daemon rollback, then found that failed
helper promotion restored the old image but started a previously stopped helper.
The fixture retained its strict stopped-disposition assertion. Product correction
`1ce24815941e12c56c9dcb5565f20616abeceeb5` is now integrated. Native run
43148506260 is queued at `9c000273e4a91e1d8c9850a8d089a10affb11973`; its result
is pending. Independent profiles now continue only after confirmed cleanup of a
failed case, and any required failure still fails the job.

The fixture uses the delivered adoption supervisor, real CLI, canonical adoption
envelope, initial-helper boundary and package migration. It does not implement a
second adoption or signature mechanism. Genuine prior transactions reuse the
delivered signed stage/grant producer with the actual native manager.

## Exact workers and incompatible format

The cohort consumes the authoritative append-only published-worker registry and
`release_workers.py` selection/start/verify/stop functions. It checks archive
identity, copies owned executable inputs, and retains every applicable exact
binary digest plus actual custom source images. The observer verifies kernel
process/image identity and fresh daemon-routed token I/O. Published versions newer
than the actual source do not enter its cohort.

Local preparation at `6e1a06949ee7c90f6be99f6783c5ea6d8894461f` passed the actual
measured transition and rollback inputs for 9001 to 9002 with 122 unique workers,
and 9002 to 9003 with 123. Both sets include the 121 published images and the
applicable actual supplemental source images. These are input measurements,
not hosted native-manager chain acceptance. All current registry protocols are
1; no other protocol coverage is claimed.

The original 9001 reader accepts the real waypoint. The original waypoint reader
accepts a genuinely incompatible schema 2 target. A signed negative control feeds
the same schema 2 manifest bytes to the original reader in its supported epoch;
it rejects them with `manifest schema 2 is unsupported`. Raw proof receipts,
rollback observations and original-reader output remain in the report directory.

The native chain installs the actual intermediate daemon and helper, keeps its
original attachment and exact cohort, and checks the final successful activation
or rollback. The failed case also restarts the real daemon and checks durable
failed-edge retention and unchanged installation request/generation. This native
chain has not yet run with the current complete producer inputs.

## Reusable job contract

The caller uses `.github/workflows/installed-catchup.yml` with `contents: read`
and `actions: read`. Unit 3 owns the caller wiring in `ci.yml` and `release.yml`.
Without inputs, the workflow runs the published-pair adoption matrix. To add the
signed native cohort/format matrix, supply all of:

- `worker_artifact_prefix`, normally `update-native`.
- `worker_artifact_run`, an exact positive Actions run ID.
- `worker_artifact_source`, the actual preparation checkout SHA recorded in
  `tools/preparation.json`, which may differ from the run head on a pull request.

Intake requires successful completed preparation jobs and exactly one live
artifact for each of `linux-amd64`, `linux-arm64` and `darwin-arm64`. It verifies
the native candidate-reader source, platform, clean build and actual image hash.
It also requires `tools/preparation.json` schema 1, its actual clean source and
platform, and matching SHA256 for candidate, worker-probe, startup-fault-candidate
and boundary. The run head is recorded as context and is not inferred to be the
preparation source. A queued run or partial artifact set cannot pass intake.

## Cleanup and remaining work

Physical socket-root preflight checks every generated path before setup, including
the longest retained-worker and temporary-daemon paths. It rejects overlong roots,
symlink-expanded roots and an unmounted requested data drive. Per-operation
timeouts remain bounded. Teardown is installed before process/service creation.
Unconfirmed service or process cleanup retains scratch and fails the proof.

The 21 focused controls pass, including real-manager normal and exception cleanup,
socket bounds, verified acquisition, exact cohort inputs and complete pinned
artifact intake. The tagged exporter compiles. The full push quality gates pass
with no new or stale baseline findings.

Still required before this is the complete Unit 8 gate: current native cohort
chain/waypoint results, three genuinely installed ordered hosts with a failed
canary, sleeping host and release burst, durable helper interruption controls,
and publication execution against the draft's real artifacts/signatures. A reboot
must report interrupted RAM sessions honestly. An actual first-published updater
baseline can only be added after publication.

Small evidence and reproducers are retained under
`/work/reports/mesh-auto-update/unit8-installed-catchup/`. Test binaries, duplicate
archives and scratch are removed after their consumers stop. The parent owns
report retirement and prepared-tree removal after review and merge. The live
fleet is not involved in these tests.
