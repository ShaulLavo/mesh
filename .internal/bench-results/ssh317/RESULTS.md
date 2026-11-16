# SSH command reuse

Status: Approved. Implementation is ready for independent review for issue 317;
the issue remains open until the PR merges.

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

## Final source and controls

The final Go implementation is `14b0ae6191250da264ce4e14cb1ecde4bb389930`.
`after.json` contains the final normal, uninstrumented `mesh_integration` build
and its baseline binary fingerprint. Load averages started at 40.87, 41.07, and
31.68 on a 28-logical-CPU PC. These timings are noisy, not a quiet-machine result.

| Mode | Warm p50 | Warm p95 | Four-way p50 | Echo first byte p50 |
| --- | ---: | ---: | ---: | ---: |
| Mesh before | 13.86 ms | 98.56 ms | 13.10 ms | 55.45 ms |
| Mesh with helper | 10.60 ms | 141.09 ms | 13.29 ms | 10.71 ms |
| OpenSSH fresh | 122.76 ms | 156.97 ms | 134.07 ms | 119.43 ms |
| OpenSSH multiplexed | 6.19 ms | 17.26 ms | 6.70 ms | 5.02 ms |

The balanced 40-pair comparison has median ratio 0.722 and median difference
-3.70 ms. The high p95 values do not establish a tail-latency improvement.
Both fresh and multiplexed SSH comparisons pass the existing generous promise
policy. Mesh remains about 4.4 ms behind warm OpenSSH multiplexing at the median.
The one cold call takes 219.4 ms with a helper launch and 181.8 ms before;
one sample under this load does not establish a cold-start distribution.

For the 142 paced calls, total measured CPU is 1.914 seconds with reuse and
2.438 seconds before, including helper, CLI, daemon, and remote children. The
helper contributes 0.100 CPU seconds, 35,588 KiB RSS, and nine descriptors.
Earlier checkpoints measured about 37 MiB RSS. All 100 separate warm CLI calls
observe the same one live server TCP inode and check their output and exit status.

The unpaced four-way batch accepts 20 and denies 20 calls with reuse in 0.184
seconds. The old CLI accepts 19 and denies 21 in 0.121 seconds. Pacing-inclusive
wall times are 21.40 and 21.77 seconds. These results preserve the admission
limitation and make no sustained-throughput claim.

Cancelled setup is bounded by 16 active or abandoned requests per destination,
eight live transports including those being closed, and 128 outstanding setup
operations across all keys. Thirty-two accepted IPC requests bound pending
headers and their received descriptors. Failed initial dials use a key-scoped,
one-second cooldown. Execution is never retried.

Barrier tests exercise the actual CLI command through Unix descriptor IPC.
They prove it returns while confirmation is still withheld, sibling stdout and
stderr remain usable, late channel opens cannot launch, and an already-sent exec
returns uncertainty without replay. Withheld opens count toward the bound and
are reclaimed when the last active sibling leaves. The tests join setup and
transport goroutines and check counters return to zero.

## Verification history

- Focused changed tests pass three race-detector repeats. The final full changed
  packages pass `go test -race -p 2 -timeout=15m ./internal/cli ./internal/sshd`.
- The clean full suite passes `go test -race -p 2 -timeout=20m ./...` before the
  final bounded setup correction; the changed packages are rerun afterward.
- `go mod tidy -diff`, the lint gate, Linux build, local Darwin ARM64 cross-build,
  and generated CLI reference pass. The gate reports zero new or stale findings.
- The full integration run passes all cases except the old OpenSSH scratch-root
  test. The corrected packaging integration then passes on final source.
  Final pooled SSH integration passes, including real idle transport/helper/socket
  cleanup. The existing interactive SSH integration passes twice on final source.
- The original dirty-source run fails the two clean-source release fixture checks.
  Those attempts remain failures. The clean run passes the release checkpoint.
- The first full race run fails command-name parity and a fleet-sharing writer
  admission deadline. Reserving the helper name fixes parity; fleet sharing passes
  the bounded full-suite rerun without a test change. The first new IPC barrier
  race run finds concurrent SSH stdin closure; giving the copier sole ownership
  fixes it, and three race repeats pass afterward.
- The benchmark attempt with the shared fixture's four-second startup wait fails
  before sampling, with no listener. Cleanup joins all children and removes state.
  The benchmark now uses its existing 120-second hang cap for fixture readiness;
  the preserved failure is `final-boundary-startup-failure.json` in the report
  directory. The subsequent final run passes and verifies cleanup.

The real OpenSSH test selects an existing scratch root whose ancestors satisfy
StrictModes. It still rejects a deliberately unsafe ancestor. Three runs with
`MESH_SHORT_TMP=/tmp` pass using a trusted root; authorization is never bypassed.

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
are removed after their final consumers stop. The actual-host check finds no
process running a task binary, removes the task scratch root, and verifies the
original integration root is gone. `cleanup.json` in the report directory records
the remaining small results and their parent-lane cleanup owner. Shared caches
and pre-existing services remain intact.

## Separate admission follow-up

The current IP-based session token bucket materially limits agent bursts. Keep it
for this change. A separate measured design can evaluate per-device active-command
and global process/channel limits while retaining connection/login protection.
That proposal must account for queued requests and slow consumers before changing
protection. It is not implemented here.
