# Exact retained worker coverage

The publisher retains the runtime's exact executable digest contract. It does
not substitute another build with the same protocol or worker source.

`scripts/upgrade-policy.json` pins the initial published version list and its
immutable history floor. Selection adds every later published manifest, even
when worker source is unchanged, because each full executable has its own
identity. The generated `mesh-workers.json` adds the current candidate only
after its actual native artifact and worker control exist. The workflow signs
that registry. Future publications authenticate the previous registry and
reject entry removal, artifact replacement, or protocol substitution.

`mesh-release-routes -operation select` emits `workers` records containing
`version`, `commit`, `protocol`, and the complete manifest `artifact` identity.
Native preparation writes `retained/workers.json`, schema 1, with `platform`
and `workers`, adding each verified binary path relative to that file's parent.
This is the shared input for the installed native harness.

`scripts/fixtures/release_workers.py` provides `applicable`, `start`, `verify`,
and `stop`. Applicability includes published versions no later than the actual
source, plus the actual source or pinned custom binary. It never requires an
older daemon to serve workers from a future release. The helper starts the
actual archived worker entry points, records their worker and shell PIDs, and
keeps those processes alive. At each observation, daemon-routed token I/O and
`TestRetainedWorkerCohortIdentity` check exact process, shell, protocol, and
kernel-observed image identities. The test executable is built once per native
platform with the other proof tools. `stop` owns cohort teardown.

The producer observes the cohort after candidate startup, candidate restart,
and every existing helper rollback boundary. It adds only the surviving
observed digests to the separate rollback proof. The schema 1 forward receipt
and the migration, actual startup-fault, and helper evidence stay intact.
Publication rejects a source proof missing any applicable registry digest.

## Complete cohort measurement

The native Linux v0.1.249 trial used all 71 applicable published binaries, not
just seed representatives. It passed 29 cohort observations, including all
copied migration states and real startup interruption/failure sides, and
emitted exactly 71 unique worker digests. Total proof time was 261 seconds.
Peak observed cohort RSS was 2,199,268 KiB across 142 worker and shell
processes. This is summed cohort RSS, not whole-host memory or PSS.

The trial's scratch directories and process consumers were checked absent on
the execution host. Its small proof, observation, and resource results remain
under `/work/reports/mesh-auto-update/unit3-publisher/cohort-trial` for review.
The initial setup attempt stopped before the proof because GNU time was absent;
its scratch was removed. The successful measurement used shell elapsed time
and the cohort's actual process observations. No deadline was increased.

Native proof jobs size the matrix from the largest full-source count across
platforms, including matching pinned custom binaries. Duplicate pinned names
are counted once, using the same selector as acquisition. Each shard owns at
most one full published or custom proof, and the runner refuses an undersized
matrix before invoking a proof. Forward-only sources are distributed over the
same shards. Every source is assigned exactly once. Original candidate,
startup-fault, boundary, and observer executables remain built once per
platform. Existing receipt, packet, worker-count, and registry byte bounds fail
closed. Exceeding them requires a reviewed format change, not truncation.

The broader native matrix remains required. A Linux cohort measurement does
not prove Darwin arm64 or Linux arm64 coverage.
