# Scratch Linux benchmarks

`run.py` uses Python's standard library and a separately built Mesh binary. It
creates a fresh HOME, config, state, app-data root and unused tailnet port for
every session count. Unix control requests address only that daemon. It never
installs a binary or calls the installed daemon's socket. SSH/HTTPS/serving stay
disabled. Workers are killed through the scratch socket before daemon shutdown;
the Pi launcher additionally checks exact executable/script paths in `/proc`.

## Reproduce

Run from the checkout being measured, with Go 1.27 and Python 3.13+ available.
The build embeds the checkout's latest release tag. These are unstripped Go
builds, so compare like builds; startup numbers are not the installed release's
stripped binary size. No production code is modified by the harness.

```sh
export PATH="$HOME/.local/share/mise/shims:$PATH"
scratch=$(mktemp -d /work/tmp/mesh-m5-XXXXXX)
heavy=/work/platform-production/heavy/current/run.js
bun "$heavy" --class build mesh-bench-build -- scripts/bench/build.sh "$scratch/native"
bun "$heavy" --class bench mesh-bench-local -- python3 scripts/bench/run.py \
  --binary "$scratch/native/mesh" --commit "$(cat "$scratch/native/commit.txt")" \
  --go-version "$(cat "$scratch/native/go-version.txt")" --output "$scratch/omarchy.json"
bun "$heavy" --class build mesh-bench-arm64 -- scripts/bench/build.sh "$scratch/arm64" arm64
bun "$heavy" --class bench mesh-bench-pi -- scripts/bench/pi.sh "$scratch/arm64" "$scratch/results"
# Copy the JSON/Markdown results before removing your scratch root.
rm -rf -- "$scratch"
```

The Pi run cross-builds locally, transfers to `/tmp/mesh-m5-XXXXXX`, measures
0/5/20 sessions, collects JSON and Markdown, verifies no scratch processes remain,
and removes that directory. Its 300-second timeout sends TERM first so Python can
clean up; an independent EXIT cleanup covers interrupted SSH connections. Keep
runs short: the Pi also drives the owner's TV.

Defaults: 3-second settlement, 30-second idle window, five repeated list/CLI/attach
samples, a 1 MiB ANSI build-log burst on the first worker. The workload prints
32 KiB to prime each screen/ring, then blocks quietly on input. Idle worker RSS
excludes the Python child. Attach measures the control acknowledgement and then
the full repaint frame; throughput requires exact byte count and contiguous
terminal sequence numbers. This is local Unix transport, not network latency.

CPU is `/proc/PID/stat` ticks divided by monotonic elapsed time, as a percentage
of one core. RSS includes shared executable pages: worker RSS totals must not be
interpreted as unique fleet memory. Short windows can quantize CPU to zero. CLI
wall/CPU/peak-RSS come from `wait4`; samples are warm-cache fresh processes.

SQLite writes are committed WAL transactions and page frames, sampled at 5 Hz
from the WAL-index's committed frame count. Header salts detect checkpoint/reset;
stale/uncommitted tails are excluded. Frames are page writes, not SQL statement
counts or bytes reaching the SD card. Rates are extrapolated from the recorded
window; this is a baseline, not plan 08's ten-minute persistence or hour-long
watch-soak gate. Very fast complete WAL resets between observations can be missed.
The committed Pi run uses `/tmp` on tmpfs; WAL counts measure logical persistence
activity, and the timings do not characterize SD-card latency or wear.

`--list-fields '{"someAdditiveOption":true}'` lets another lane compare a lean
catalog without changing this baseline's full-list request. Labels include source
commit, architecture, Go version, kernel, host, timestamp, window and binary size.
The committed `results/origin-main-9e3f62b/` measurements use production source at
`9e3f62b56405bdda7a90a32b0acfe3d628fd9208`; only test/harness files were added when
those measurements were collected. Later main integrations do not change that
pin or relabel the results. A new run measures the checkout being built.

## Profiles

```sh
bun "$heavy" --class build mesh-profile-build -- scripts/bench/build.sh "$scratch/profile" amd64 profile
bun "$heavy" --class bench mesh-profile -- scripts/bench/profile.sh "$scratch/profile" "$scratch/profiles"
```

The build overlay adds CPU/heap/alloc profiling to `main` only in this scratch
binary. Runtime profiles cover a 16 MiB burst through a real daemon, worker, PTY,
ring and x/vt screen. The profiler stops after five seconds; its forced GC and
200 ms CPU-profile shutdown make instrumented timings unsuitable as baselines.
Microbenchmarks cover ring write/replay, screen parsing/save, quiet checkpoints,
relay/attachment queues, unchanged picker/inspector updates and keepalive
Ping/Pong, plus CLI command-tree construction. The Ping benchmark measures each Ping's timeout/socket work without waiting
15 seconds between operations. Loopback keepalive results are not WAN RTTs.

`GODEBUG=inittrace=1` captures CLI package initialization. At the committed
baseline pin hashing runs during package init, before a profile started in `main`.
Current main hashes lazily; the release benchmark creates and invokes a fresh
identity reader each iteration so the process cache does not hide cold hashing.
It measures its **test executable**, including reader creation/open/hash/close,
not CLI startup. The baseline benchmark also reread build metadata per iteration;
the current implementation reads metadata once during package init, so these
operation costs differ. Keep the original profiles pinned, and use actual Mesh
startup measurements for before/after claims. Profiling changes no shipped source
and proposes no performance fixes.

`integration/bench_harness.sh` checks fragmented frames, limits, `/proc`, real
SQLite commit/reset counting, and a real 0/2-session lifecycle/throughput smoke
run. Full baselines remain opt-in rather than adding Pi access or long workloads
to CI.

For short Pi microbenchmarks, cross-compile the test binaries into the same build
folder before running `profile-pi.sh`; these run serially at 200 ms per benchmark,
collect CPU and heap profiles, and keep test scratch inside the remote root:

```sh
bun "$heavy" --class build mesh-pi-tests -- bash -c '
  for p in worker tui daemon transport session terminal release cli; do
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o "$1/$p.test" "./internal/$p"
  done
' _ "$scratch/arm64"
bun "$heavy" --class bench mesh-pi-profile -- scripts/bench/profile-pi.sh "$scratch/arm64" "$scratch/pi-profiles"
```
