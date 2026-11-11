# Host updater implementation

Unit 6 keeps `DefaultHostMode` manual. It adds no enrollment, public command,
installer change or live fleet activation.

## Authority and recovery

Host calls bind `Engine.WithHostOrigin` in process. The marker cannot arrive in
request JSON, and a configured acceptance always runs, including when
`Request.Automatic` is nil. Stage publication after download, retry and grant
all pass through the same policy fence. Manual calls retain the existing
coordinator consent checks.

The installation lock precedes the authority lock. Policy and ownership writers
never take the installation lock while holding authority. Health inspection
runs before authority acceptance. Policy changes increment the revision under
authority, and acceptance holds that lock through the journal commit.

Ownership saves the exact request, generation, policy revision, edge and
installation settings before the first stage. A restart rechecks signatures,
the current source, rollback coverage, the helper and every retained worker.
An issued grant finishes independently of policy changes. Reconciliation runs
in manual mode and records the result before clearing ownership.

Ledger entries have separate bounded files and an index keyed by request and
generation. Reads return at most 64 entries. A crash after entry publication,
after head publication or before ownership clearing can repeat safely.
Recording the last phase also permits an explicit manual retry of a previously
failed transaction to record its later commit. No fixed history count removes
entries. Unit 7 owns dependent acknowledgements and subsequent ledger pruning.
Adoption confirmation and its operation/pin milestones have separate durable
storage, so normal ledger pruning cannot erase them.

Rollback quarantine uses one private record per exact source, destination and
platform. Changing the final target does not remove it. Route cursors retain
only the currently encountered exclusion; the persistent record is checked
again for every selected edge. A different commit prunes the old source's
failed-edge records. A failed rollback requires attention and starts no daemon
restart loop. Preactivation failures back off from 15 minutes to six hours.

## Authenticated source and hop

The HTTP adapter uses the actual Sigstore verifier and authenticated trust
refresh. Metadata binds exact manifest bytes. Each hop rechecks its exact
forward receipt, complete rollback proof and retained worker implementations.
Worker checks run again at stage, retry and grant acceptance.

The source adapter uses shared `release.UpdateMetadata.Updater` and
`release.UpdaterReader`. Normal reader coordinates equal publication
coordinates. A waypoint is an old-epoch anchor whose marker names the next
epoch's anchor 0. Missing, altered, skipped-epoch or inconsistent markers fail
closed. The compiled reader epoch must equal its authenticated source marker.
The hidden, read-only `mesh update-reader` command verifies byte-preserving
offline packets using the actual executing source identity. Its exact contract
and executable test evidence are in [the reader evidence](17-host-reader.md).

## Verification

Runs use Go 1.27.4 and the shared PC runner. The reproducer and job records are
under `/work/reports/mesh-auto-update/unit6-host-updater/`. Its wrapper creates a
short physical `/work/tmp/u6-*` root, installs EXIT teardown before execution,
and isolates Mesh configuration and state. Existing caches remain active.

The four package race suites passed on implementation checkpoint `2645408e`:

```sh
go test -race ./internal/update ./internal/updateinstall \
  ./internal/updatebootstrap ./internal/daemon -count=1
```

Later focused race runs cover the source marker, waypoint coordinates, actual
state observation, request-keyed ledger and download fence refinements:

```sh
go test -race ./internal/update -run '^TestHost' -count=1
go mod tidy -diff
scripts/gates.sh
```

The signed fixtures use generated SCT, transparency-log and certificate
evidence with test-only trust material. The real engine and fake service run
two hops across updater replacement while one real worker preserves I/O.
Other cases cover quarantine across twelve newer final targets and a verified
alternative, rollback failure, off and revision races, pre-journal recovery,
ledger crash recovery, foreign manual work, adoption confirmation, legacy
consent and invalid prerequisites. The ledger page test retains 150 entries.

Three integrations passed with one temporary Mesh build:
`daemon_restart.sh`, `serving_survives_restart.sh` and
`automatic_fleet_updates.sh`. No broad CLI or integration rerun was requested
for the already recorded fixture-430 and timing observations.

One initial gate run used the runner's light class and was killed during
deadcode analysis. Repeating it in the suite class completed. Another setup
attempt exposed mise's trust check under an isolated XDG directory; using the
installed tool paths fixed the harness without changing any trust setting.
No timeout or baseline exception was added. Two existing wrapper-error
baseline entries were removed after fixing them in touched daemon code.

## Remaining gates

The corrected rollback source at `512fb62b` is integrated. The parent must
deliver corrected signing-cache source, arrange independent review and require
green CI before merge. The shared marker and original-source command contracts
are implemented. The PR remains a draft until the remaining cross-unit gates
are complete. No installed-platform or fleet activation claim is made by these
fixture tests.

The prepared tree remains for that review and belongs to the parent for final
retirement. Temporary binaries, test state and workers are removed after their
consumers stop. Runner receipts include one transient process counted after a
commit hook; its scope was subsequently confirmed absent/inactive on the host.
