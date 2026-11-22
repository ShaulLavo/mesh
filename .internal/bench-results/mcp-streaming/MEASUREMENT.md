# Native MCP streaming after the reader fix (#318, PLAN section 11)

Measured on 2026-11-22 with `bench_stream.py` (the `mcp_native` fixture: `mesh mcp` on the
client state, jobs on a second daemon over its authenticated WebSocket). Each run tails a paced
100 MiB pipe job with 1 MiB `mesh_job_output` pages, rereads the retained suffix, then checks a
stopped reader against an unpaced 100 MiB job.

Builds: before = `36cd552d` (main), after = this branch (`083c7998` content).

## Quiet machine: VPS (2 vCPU, load average 0.3), 3 runs each

| | before | after |
| --- | --- | --- |
| time to first byte (includes launch) | 0.81 to 1.53 s | 0.69 to 0.73 s |
| first offset read | 12 MiB, 13 MiB, 0 | 0, 0, 0 |
| bytes read while tailing 100 MiB | 10 MiB each run | 21, 27, 21 MiB |
| tail reader throughput | 1.4 to 1.5 MiB/s | 2.5 to 3.4 MiB/s |
| retained 8 MiB reread | 8.2 to 9.9 MiB/s | 7.8 to 9.0 MiB/s |
| stopped reader: first cursor | `logs.cursor_expired` | `logs.cursor_expired` |
| stopped reader: earliest offset, retained | 96468992 (100 MiB - 8 MiB), 8 MiB | same |
| retryable errors | 0 | 0 |
| worker CPU, peak RSS (tail job) | 0.7 to 1.4 s, 42 to 43 MiB | 3.3 to 3.7 s, 44 MiB |
| frontend CPU, peak RSS (whole bench) | 5.5 s, 59 MiB | 8.2 s, 57 MiB |
| daemon CPU, peak RSS (whole bench) | 5.1 s, 79 MiB | 7.2 s, 77 MiB |

The producer writes about 12 MiB/s here, faster than a 1 MiB page round trip (about 110 ms
median), so the tailing reader falls more than the 8 MiB retention behind and the host reports
each jump as a gap (`logs.cursor_expired`, then the earliest cursor). That is the designed
bounded-retention behavior. Before the fix the reader mostly waited for the writer's lock
between pages; after it, the reader gets about two and a half times as many bytes and always
starts at offset 0. The extra worker, frontend and daemon CPU is the extra bytes read and hashed.

Data: `vps-before-result.json`, `vps-fixed-result.json`.

## Loaded machine: PC (load average 30 to 40 from other jobs), 1 run each

| | before | after |
| --- | --- | --- |
| output reads in the run | 152 | 110 (100 of them the tail) |
| reads that failed waiting for the stream lock | 129 (`state.unavailable`) | 0 |
| bytes read while tailing | 19 MiB, 4 gaps | 100 MiB, contiguous from 0, EOF |
| per-read time (reads with data) | median 0.06 s, but most reads failed after about 2 s | median 3.7 s, p90 12 s, max 20 s |

On this host each commit and each reader confirmation waits on fsync behind other jobs' disk
traffic, so the whole tail took about 580 s (0.17 MiB/s) and the producer was held to the same
pace. Every read returned data. An intermediate build without the final-flush fix showed the
second loose end: the job exited with about 0.8 MiB still queued, the final commit outlasted the
worker's 5 s budget, and the capture was recorded as `not_recorded` with the stream never
reaching EOF (`pc-loaded-first-fix-calls.jsonl`, 233 empty reads after the last byte). With the
30 s pipe flush budget the run above reached EOF at 100 MiB. Its stopped-reader step then hit
the bench's own 60 s `mesh_job_wait` cap, because the unpaced 100 MiB job took longer than that
on this disk; the stopped-reader check is covered by the VPS runs.

Data: `pc-loaded-before-calls.jsonl`, `pc-loaded-first-fix-calls.jsonl`, `pc-loaded-fixed-calls.jsonl`
(one line per output read: call seconds, start offset, bytes, EOF, error code and message).

## Reproduce

```sh
root=$(mktemp -d /work/tmp/mcp318-XXXXXX)
python3 -B .internal/bench-results/mcp-streaming/bench_stream.py "$BIN" "$root" "$root/result.json" 3
```

Stop it with SIGINT (not SIGTERM), so the fixture teardown runs.
