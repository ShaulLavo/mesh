# SSH command reuse

Status: Approved. Implementation and local verification are in progress for issue 317.

## Baseline and bounded profile

The baseline is main `e943c1f9ceebb2402d12b6c8b3c10c6283667443`. Builds use Go 1.27.4
with the `mesh_integration` tag. All execution uses the installed heavy runner
with `--host local`. The isolated listeners use 127.0.0.2 on this Linux x86-64 PC,
the same account and network, and a pinned host key. Other PC jobs were active.
No fleet host, canonical daemon, service-manager unit, identity, or network
configuration was changed.

Actual separate CLI processes run `/bin/true`. The first call measures one cold
helper startup; 100 subsequent calls measure warm operation. Four-way batches
measure concurrency. Echo commands measure first output. Every successful
sample checks exit status and exact stdout and stderr. Latency sampling sleeps
110 ms per invocation outside the timed region to respect the unchanged server
limit of 10 sessions per second with burst 20. Reports also include total paced
wall time and an unpaced 40-command, four-way batch with admission errors.

The matched uninstrumented checkpoint measured:

| Mode | Warm p50 | Warm p95 | Echo first byte p50 |
| --- | ---: | ---: | ---: |
| Mesh before | 13.27 ms | 14.45 ms | 12.83 ms |
| Mesh with helper | 10.08 ms | 11.04 ms | 9.56 ms |
| OpenSSH multiplexed | 6.08 ms | 6.62 ms | recorded in JSON |

Balanced random before/after pairs had median ratio 0.772 and median difference
-3.02 ms. Shared-load outliers remain visible in the raw samples. Fresh OpenSSH
is also measured and retained; its normal account-shell startup is included.

A temporary, identically instrumented before/after build attributes about 2.91 ms
to a fresh SSH handshake. Median key parsing is 0.022 ms; pool key parsing plus
fingerprinting is 0.032 ms. Unix connect, private pipes, and descriptor/header
send take 0.035, 0.026, and 0.039 ms. Channel creation takes 0.162 ms, the launch
request 0.099 ms, and remote preparation plus process start about 0.181 ms.
Go initialization reaches 5.3 ms before main. This issue keeps that startup cost
and the remaining OpenSSH multiplexing gap explicit.

For 142 paced calls, the uninstrumented CPU account is 1.861 seconds with reuse
and 2.330 seconds before. These totals include CLI processes, the live helper,
the daemon, and remote child processes. The helper contributes 0.08 seconds,
37,412 KiB RSS, and nine descriptors. Its memory is an explicit tradeoff.
One server TCP inode remains throughout all 100 warm calls. The profiled warm
case has one authenticated handshake; the old CLI authenticates for each call.

The unpaced batch accepts 18 and denies 22 calls with reuse in 0.084 seconds.
The old CLI accepts 19 and denies 21 in 0.114 seconds. These are burst results,
not sustained throughput claims. Pacing-inclusive wall time is 16.84 seconds
with reuse and 17.33 seconds before. Existing admission protection remains
unchanged.

The first unmatched after run showed little median improvement and worse tails.
That result triggered the bounded profile, rather than a speed claim. Both
results remain in `/work/reports/mesh-ssh317`.

## Reproduce

Build a normal local integration binary from the desired recorded revision:

```sh
bun /work/heavy-runner/current/run.js --host local --class build ssh317-build -- \
  go build -tags mesh_integration -o /work/tmp/ssh317/mesh ./cmd/mesh
bun /work/heavy-runner/current/run.js --host local --class bench ssh317-bench -- \
  python3 -B scripts/bench/ssh_command.py --agent-commands \
  --binary /work/tmp/ssh317/mesh --baseline-binary /path/to/baseline \
  --scratch-parent /work/tmp --pairs 20 --output /work/reports/mesh-ssh317/result.json
```

The benchmark owns descendant cleanup before setup, then removes its private
state after every child joins. It records binary fingerprints, version, platform,
load, argv, connection policy, resources, and cleanup. Promise comparisons use
the established generous ratio, absolute margin, and sign-test policy. Correctness
tests use outcome assertions with hang caps; they do not assert speed.

Temporary phase instrumentation is removed from normal source. Its small
reproducer and phase JSONL remain in `/work/reports/mesh-ssh317` with the matched
profile and uninstrumented JSON. Keep those results until review and merge, then
the parent lane owns their archival or removal. Task binaries and source copies
are disposable and are removed after their final consumers stop.

## Separate admission follow-up

The current IP-based session token bucket materially limits agent bursts. Keep it
for this change. A separate measured design can evaluate per-device active-command
and global process/channel limits while retaining connection/login protection.
That proposal must account for queued requests and slow consumers before changing
protection. It is not implemented here.
