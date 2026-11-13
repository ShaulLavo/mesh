# Diagnostic transport lifecycle observer

Status: Approved for implementation. Hosted execution requires independent source
and vehicle review, then the parent runs one focused Linux AMD64 chain-final profile.

`mesh_installed_fixture` builds enable this tooling only when the fixture sets
`MESH_TRANSPORT_LIFECYCLE_DIR`. Ordinary binaries never configure it. Fixture
drivers opt in with `MESH_TRANSPORT_LIFECYCLE=1` and an explicitly diagnostic clock.
The workload and provenance carry `transportLifecycle`. Such measurements cannot
be frozen or grant installed acceptance, including through `SampleRef.eligible`.
Exported instrumented samples have `eligible: false`; `diagnostic_complete`
separately records completed, cleaned diagnostic measurements.

Each current daemon records its own PID, running-image digest, source commit and
version once. Its private JSONL file has connection IDs, numeric TCP endpoints,
wall and monotonic observation times, fixed lifecycle stages, four-character
session IDs and allowlisted error classes. No raw errors, peer keys, certificates,
headers, addresses containing hostnames, payloads or terminal input are recorded.
The existing authorization callback executes exactly as before, with its result
observed once during TLS verification. The bound access class and the existing
pre-acceptance grant-current check are observed without another callback.
No retries or admission rules change.

The recorder has a 256-event nonblocking queue, 4096 records and 2 MiB per process.
At most 32 files are admitted in an owned case directory. Queue loss, missing or
partial records and incomplete shutdown invalidate coverage. Events stop after
the daemon has joined its connection handlers. A 20 ms final drain bounds optional
writer teardown. Observer I/O failures do not propagate into transport operations.

Before a selected worker cohort starts, the diagnostic case must prove the actual
original client's post-adoption attachment. The gate checks its captured published
image, live PID, adopted daemon mapped image, matching process metadata, successful
TLS/authorization/acceptance/attach events and the original client's TCP socket
inode/tuple. It uses the existing successful marker/I/O result. Its at-most-one-second
passive log wait consumes the existing diagnostic clock; it never retries a marker.
Successful later inspections repeat coverage checks for each replacement daemon,
with each process image hash cached after its first observation.

At cleanup, raw bounded lifecycle files are retained even when coverage is
incomplete. The original product exception is preserved. A successful product case
with incomplete observer coverage fails as coverage-incomplete, not nonreproduction.
The approved helper observer's sampled inode/device metadata can accompany a
helper exception without rereading or changing the exception.

Render the independent vehicle with:

```sh
python3 scripts/fixtures/transport_lifecycle_vehicle.py /work/path/vehicle.yml
```

Recorder construction, configuration and disk writing compile only with the
`mesh_installed_fixture` tag. The observer tests use that same tag and run in the
required native-socket CI job. Normal builds keep the inactive hook behavior.

The parent installs that file as the already registered
`.github/workflows/rollback-proof-investigation.yml` on an independent vehicle
branch. `source_sha` separately pins the reviewed source commit. The vehicle
prepares genuinely signed current-source assets and runs only the complete AMD64
chain-final profile, with original v0.1.249 and every authoritative selected group.
It runs neither order workloads nor additional profiles, and preserves existing
job, source, product, I/O, soak, rollback and group deadlines.
