# Diagnostic transport lifecycle observer

Status: Reusable diagnostic tooling. Hosted execution requires independent source
and vehicle review and a coordinator-approved case. Current interruption evidence
and its disposition are recorded below.

`mesh_installed_fixture` builds enable this tooling only when the fixture sets
`MESH_TRANSPORT_LIFECYCLE_DIR`. Ordinary binaries never configure it. Fixture
drivers opt in with `MESH_TRANSPORT_LIFECYCLE=1` and its explicitly marked observer workload.
The workload and provenance carry `transportLifecycle`. Such measurements cannot
grant installed acceptance.
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
passive log wait consumes the existing parent hang cap; it never retries a marker.
Successful later inspections repeat coverage checks for each replacement daemon,
with each process image hash cached after its first observation.

At cleanup, raw bounded lifecycle files are retained even when coverage is
incomplete. The original product exception is preserved. A successful product case
with incomplete observer coverage fails as coverage-incomplete, not nonreproduction.
The approved helper observer's sampled inode/device metadata can accompany a
helper exception without rereading or changing the exception.

Render the earlier chain-final vehicle with:

```sh
python3 scripts/fixtures/transport_lifecycle_vehicle.py /work/path/vehicle.yml
```

Recorder construction, configuration and disk writing compile only with the
`mesh_installed_fixture` tag. The observer tests use that same tag and run in the
required native-platform authentication CI job. Normal builds keep the inactive hook behavior.

Earlier captures used `.github/workflows/rollback-proof-investigation.yml` on an
independent vehicle branch; that registration has since been retired. The current
interruption vehicle uses registered `ci.yml` with `client_interrupt=true`.
For the earlier rendered vehicle, `source_sha` pins the reviewed source commit. It
prepares genuinely signed current-source assets and runs only the complete AMD64
chain-final profile, with original v0.1.249 and every authoritative selected group.
It runs neither order workloads nor additional profiles, and preserves existing
source, product, I/O, rollback and group outcomes within generous hang caps.

## Original-client interruption, November 15, 2026

Managing classified the early client exit as not reproducing after the faithful
instrumented replay. This is not a product fix. Keep the strict original-client
resume assertion in normal CI. Run PR 460's normal CI once more; if that early
exit recurs, stop and return both runs' evidence without an automatic retry.

- [Ordinary failure, run 43649781233](https://github.com/ShaulLavo/mesh/actions/runs/43649781233/job/132627688178):
  the v249 client exited before its resume marker while its worker, shell and
  listener survived. Artifact 14137736968 is 96,961 bytes, SHA-256
  `07645b7963ceaa419dc8c8f8a26a3ada282add33f96f5419142e6b1d4d63eaf8`.
- [Instrumented replay, run 43666589946](https://github.com/ShaulLavo/mesh/actions/runs/43666589946):
  the original client reconnected, authenticated, resumed and completed fresh
  shell I/O. It was alive at cleanup; its later SIGKILL preceded the server reset.
  All three traces had zero drops. Artifact 14141704071 is 226,387 bytes, SHA-256
  `0066029d0af8fe4e9526fd6403951727943f2dfeffa1d3688425ef193fc3cd83`.

The replay failed afterward because the observer compared a worker's PPID across
retirement of its daemon parent. Worker reparenting is expected. The corrected
observer keeps PID, birth, UID and image checks, records before/after identities,
and retains parent checks for the client, shell and listener. The early assertion
also prevented the final coverage record; raw successor events survived.

Ptrace changes scheduling, and the original client discards its final Go error
chain. The replay therefore establishes no cause for the ordinary failure.
Both archives were verified against API sizes and digests and passed ZIP checks.
The replay reports canonical units, signer, proof descendants and scratch absent.
Detailed evidence is retained by the Mesh lane in `repair-pr460` reports. No
further diagnostic replay is implied by this disposition.
