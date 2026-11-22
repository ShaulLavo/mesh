# Native MCP streaming (#318, PLAN section 11): not measured, readers starve

Attempted on 2026-11-22 on the PC (load average 30 to 60 from other work), Mesh `02700c75`
plus this PR's ownership fix, with `bench_stream.py` (the mcp_native fixture: `mesh mcp`
on the client state, jobs on a second daemon over its authenticated WebSocket).

## What happened

A paced producer (`head -c 1 MiB | tr '\0' a` 100 times with 20 ms sleeps) ran as a
`mesh_exec` pipe job while one reader tailed stdout with `mesh_job_output` (1 MiB pages,
`timeout_ms` 30000). `tail-calls-2026-11-22.jsonl` is every read the reader made:

- 118 reads over 345 s, 0 bytes returned.
- Every read failed with `state.unavailable`:
  `execution control: read execution page: lock.go waitLock: context deadline exceeded`
  (median 2.7 s, max 8.2 s per call; the call's own `timeout_ms` was 30 s).
- The producer took 5 min 52 s for 100 MiB. An earlier trial took 7 min 9 s, and its reader
  was still getting no data minutes after the job exited.

Throughput, time to first byte, the stopped-reader gap check and CPU/RSS could not be
measured, because no read succeeded.

## Suspected cause

`internal/session/execution/capture.go`: pipe capture uses `OfferExact`, so the child is
held back to the disk writer's pace and the queue stays full. `Capture.flush` then loops
straight into the next `commit`, which takes the exclusive `stream.lock` microseconds
after the previous commit released it, and holds it across load, settle, publish
(with fsync) and prune. Readers (`streamStore.readPage`, `lockShared`) poll for a shared lock
with `LOCK_NB` every 10 ms (`Files.waitLock`) and almost never land in that gap. On a
quiet disk each commit is short and the queue drains, so the M2 concurrent-tail test passes.
On a loaded host a reader gets nothing until the writer goes idle.

Two more points to check:

- Why the daemon gives up after about 2.7 s when the MCP call asked for 30 s.
- Why reads still failed after the job had exited.

## Earlier bug found the same way (fixed in this PR)

Before the fix in `internal/daemon/execution_output.go`, a journal-lock timeout while
checking ownership came back as `access.denied` ("application caller does not own this
work"), telling the job's own owner it had lost the job. It is now a retryable
`state.unavailable`.

## Reproduce

```sh
root=$(mktemp -d /work/tmp/mcp318-XXXXXX)
python3 -B .internal/bench-results/mcp-streaming/bench_stream.py "$BIN" "$root" "$root/result.json" 3
```

`$root/calls.jsonl` records every output read. Stop it with SIGINT (not SIGTERM), so the
fixture teardown runs.
