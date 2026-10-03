# Explicit local update-helper recovery

Status: Approved. Live execution requires a separate owner decision, green native
CI at the containing source head, and a published release containing this command.
The PR-built helper in the native fixture is **unpublished**.

## Transaction boundary

The published v0.1.149 helper executes its own pre-commit cutover and rollback
code. Publishing a corrected daemon cannot replace that executor before its
commit. Run the corrected executable's explicit `update-helper recover` command
to repair only the local helper, then review each ordinary daemon update separately.
The approved historical daemon route remains v0.1.149 → v0.1.151 → v0.1.159.

`internal/updateinstall` owns the existing installation lock, journal, immutable
helper images and `HelperInstallation` receipt. Recovery takes `HelperConfig`,
an expected `{operation, generation, phase, originalDigest}` journal tuple, an
exact release descriptor and executable digest, the actual helper-process probe,
and an approval-idle probe. The public receipt shape remains
`{executable, servicePath, digest}`. Success adds a verified process PID and phase.
No fleet operation, alternate installer, migration or recovery state machine is
created. Recovery never removes `activation.pending`.

Recovery accepts `rolled_back`, `failed` and `rollback_failed`. It acquires the
bounded, cancellable installation lock, re-reads the entire journal, and requires
an idle approval/gate state, the original healthy daemon's mapped image, unchanged
boot identity, and the original live worker/shell identities. The replacement
must match the ordinary HTTPS release descriptor, SHA256 and executing build;
its capabilities must cover the journal, daemon, workers and prior helper. A real,
cancellable `update-helper --check-journal` subprocess checks the selected state.
Downgrades and equal-version/different-digest replacements fail closed.

For `rollback_failed`, the existing Engine's stop/restore/start path must prove
the original daemon and workers and write its genuine `rolled_back` receipt.
Recovery cannot manufacture settlement. This restoration can restart the daemon;
ordinary terminal helper repair leaves its executable and process unchanged.

Before promotion, recovery retains the exact prior receipt beside its verified
immutable image. It uses the lock-free existing helper preparation path, writes
service bytes and authoritative `installed.json` before switching `current`,
restarts the helper, and binds the service manager's actual PID to the expected
mapped image inode/device and digest. The same PID must remain stable through
final journal, approval and original-health checks. A successful restart or
journal probe alone cannot establish readiness. Existing immutable copies are
verified and reused without changing their inode.

Promotion/restart is the first owned service mutation. Readiness, restart and
cancellation failures restore the exact prior service, receipt and link, restart
it, and verify its actual executing image under the existing bounded restoration
contract. Both failure causes are retained if restoration fails. Process death
mid-promotion remains explicitly fail-closed: the retained prior receipt is
available for diagnosis, but no automatic journal or promotion healing occurs.
Stop and obtain a new reviewed recovery decision after ambiguous state.

## Root command template

These commands are a source-accurate handoff, **not live-authorized**. They must
be run locally on the selected installation through independently confirmed
access. Do not use the old installed executable to dispatch the new command.
Set the variables from reviewed records; never guess the version, digest, tuple,
state directory or paths. `fixed` is an absolute, verified executable from the
published containing release. `daemon` is the installation journal's absolute
`settings.executable`. `state` is its absolute `settings.stateDir`.

```sh
set -eu
: "${fixed:?absolute downloaded containing-release executable}"
: "${fixed_version:?published containing-release version}"
: "${fixed_sha256:?published native binarySha256}"
: "${state:?reviewed absolute Mesh state directory}"
: "${daemon:?reviewed absolute installed daemon executable}"
: "${operation:?reviewed installation operation ID}"
: "${generation:?reviewed installation generation}"
: "${phase:?reviewed rolled_back, failed or rollback_failed phase}"
: "${original_sha256:?reviewed original daemon digest}"

# Read only: compare platform/version/commit/digest/capabilities with its manifest.
"$fixed" version --json
"$daemon" version --json
jq '{phase, operation: .request.id, generation: .request.generation,
     originalDigest: .request.current.digest, original: .original.build,
     workers: .original.workers, verified: .verified.build}' \
  "$state/update/installation.json"
jq '{executable, servicePath, digest}' "$state/update/helper/installed.json"
test ! -e "$state/activation.pending"

# Separate explicit helper-recovery decision; this grants no daemon update.
"$fixed" update-helper recover --state-dir "$state" \
  --replacement "$fixed" --version "$fixed_version" --sha256 "$fixed_sha256" \
  --operation "$operation" --generation "$generation" --phase "$phase" \
  --original-sha256 "$original_sha256" --json

# Review the returned positive helper PID/digest and the actual service-manager
# registration, original daemon build, workers, shell I/O and preserved records.
# Only after separate approval of the first ordinary historical bridge:
"$daemon" update --local --version v0.1.151 --yes --json

# Read back committed journal, actual v151 daemon image, original workers/shell
# I/O, identity/database/authorized keys and the still-fixed helper image.
# Only after separate approval of the second ordinary historical bridge:
"$daemon" update --local --version v0.1.159 --yes --json

# After the v159 readback, separately review the containing release's genuine
# v159 transition receipt and approve its ordinary daemon update:
"$daemon" update --local --version "$fixed_version" --yes --json
```

The final hop requires its actual published descriptor and joined transition
receipt. Helper-only capability validation grants no direct v149 daemon jump.
Keep other machines on their current releases until the owner confirms the Mac
has completed the approved route and remote-session compatibility checks.

Use `--service-dir` only for the reviewed existing helper service directory when
it differs from the platform's normal user service directory. Every command
stops on a nonzero result. Stop before the next bridge on missing publication or
native proof, unexpected digest/schema/build/PID, active approvals or activation,
changed journal, failed restoration, missing session, failed shell I/O or a
helper downgrade. Do not increase the health timeout, delete activation files,
edit phases, force installation or skip a historical bridge.

## Native proof and limits

`.github/workflows/helper-recovery.yml` requires actual Darwin/arm64 and
Linux/amd64 execution. `scripts/prove-helper-recovery.sh OUTPUT-DIRECTORY` builds
one fixed helper as an explicitly unpublished source fixture. The harness reuses
`fixtures.published_releases.acquire` for unchanged actual v149/v151/v159 archives,
descriptors and joined transition receipts, and `fixtures.tls` for child-only
certificate controls. It never changes host trust or installed services.

Real native daemon/helper/worker processes and retained shells execute in isolated
state. External service-command providers expose actual helper PIDs and emulate
delayed outgoing registration removal and failures. The chain verifies mapped
images, worker/shell retention and I/O, identity/key preservation and database
integrity. Failed helper restart/readiness restores the prior helper. A genuine
historical failed rollback is settled by the fixed existing Engine. Native Go
tests additionally cover lock/journal races, active-state rejection, cancellation,
dual restoration errors and monotonic helper preservation.

Fake launchctl registration does **not** prove registration with the owner's
real launchd. Native CI is **not** live owner-host verification or rollout
permission. Evidence records both limitations and distinguishes public historical
artifacts from the source-built fixed-helper descriptor.
