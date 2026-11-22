# Native MCP streaming after the reader fix (#318, PLAN section 11)

Measured on 2026-11-22 with `bench_stream.py` (the `mcp_native` fixture: `mesh mcp` on the
client state, jobs on a second daemon over its authenticated WebSocket). Each run has three
phases: **tail** a paced 100 MiB pipe job with 1 MiB `mesh_job_output` pages; **reread** the
retained 8 MiB suffix after the job ends; **stopped reader**: read an unpaced 100 MiB job only
after it ends. The tail checks that pages are contiguous, and after each reported gap that the
reader resumed no earlier than where it stopped (`bytesSkippedAtGaps` sums the skipped bytes).

Builds: before = `36cd552d` (main); after = this branch at `87f684d2`.

## Quiet machine: VPS (2 vCPU, load average 0.3 to 1.3), 3 runs each

| Phase | Measure | before | after |
| --- | --- | --- | --- |
| tail | time to first byte (includes launch) | 0.81 to 1.53 s | 0.68 to 0.73 s |
| tail | first offset read | 12 MiB, 13 MiB, 0 | 0, 0, 0 |
| tail | bytes read of 100 MiB | 10, 10, 10 MiB | 24, 26, 22 MiB |
| tail | reader throughput | 1.4 to 1.5 MiB/s | 2.8 to 3.3 MiB/s |
| tail | gaps reported | 13 to 17 | 61 to 66 |
| reread | retained 8 MiB | 8.2 to 9.9 MiB/s | 8.3 to 8.8 MiB/s |
| stopped reader | first cursor | `logs.cursor_expired` | `logs.cursor_expired` |
| stopped reader | earliest offset, bytes retained | 96468992 (100 MiB - 8 MiB), 8 MiB | same |
| all | retryable errors | 0 | 0 |
| tail | worker CPU, peak RSS | 0.7 to 1.4 s, 42 to 43 MiB | 3.3 to 3.4 s, 43 to 44 MiB |
| all | frontend CPU, peak RSS | 5.5 s, 59 MiB | 8.0 s, 62 MiB |
| all | daemon CPU, peak RSS | 5.1 s, 79 MiB | 7.5 s, 77 MiB |

The producer writes about 12 MiB/s here, faster than 1 MiB page round trips (about 110 ms
median), so the tailing reader falls more than the 8 MiB retention behind, the host reports a
gap, and the reader resumes at the earliest retained byte. That is the designed bounded-retention
behavior; after the fix the reader takes more turns, so it reads more bytes and also meets the
moving retention edge more often. Before the fix the reader mostly waited for the writer's lock.
The extra CPU after the fix is the extra bytes read, hashed and handed over.

An intermediate version of this branch held `reader.lock` through every writer stream wait; it
cut the tail to 10 to 12 MiB per run in the same window where the previous build read 18 to
25 MiB, so the writer now claims a turn only after readers keep the stream for a full
`readerYieldLimit`.

Data: `vps-before-result.json`, `vps-fixed-result.json`.

## Loaded machine: PC (load average 30 to 40 from other jobs)

| Phase | Measure | before | after |
| --- | --- | --- | --- |
| tail | output reads | 134 | 100 |
| tail | reads that failed waiting for the stream lock | 121 (`state.unavailable`) | 0 |
| tail | bytes read of 100 MiB | 9 MiB, 4 gaps | 100 MiB, contiguous from 0 to EOF |
| tail | time per read with data | most reads failed after about 2 s | median 4.7 s, p90 12 s, max 21 s (about 630 s in all) |
| reread and stopped reader | reads | 18 more reads, 10 MiB | 10 more reads, 10 MiB, no errors; the stopped-reader step hit the bench's own 60 s `mesh_job_wait` cap because the unpaced 100 MiB job outlasted it on this disk (that check is covered on the VPS) |

On this host each commit and each reader confirmation waits on fsync behind other jobs' disk
traffic, so the producer is held to the disk's pace. An intermediate build of the first fix
showed the second loose end: the job exited with about 0.8 MiB still queued, the final save
outlasted the worker's 5 s attachment flush budget, and the capture was recorded as
`not_recorded` with the stream never reaching EOF (`pc-loaded-first-fix-calls.jsonl`, 233 empty
reads after the last byte). The pipe capture's own 30 s final-save budget fixes that.

Data: `pc-loaded-before-calls.jsonl`, `pc-loaded-first-fix-calls.jsonl`, `pc-loaded-fixed-calls.jsonl`
(one line per output read in every phase: call seconds, start offset, bytes, EOF, error code and
message).

## Reproduce

```sh
root=$(mktemp -d /work/tmp/mcp318-XXXXXX)
python3 -B .internal/bench-results/mcp-streaming/bench_stream.py "$BIN" "$root" "$root/result.json" 3
```

Stop it with SIGINT (not SIGTERM), so the fixture teardown runs.
