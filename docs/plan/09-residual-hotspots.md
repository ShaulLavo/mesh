# Residual terminal and checkpoint costs

Status: Approved, 2026-10-01. Execution starts after the current Plan 08 wave.
This document approves follow-up work; it starts no implementation or measurement.

## Design note

Keep the current worker, terminal wrapper, recovery writer, and TUI ownership.
First measure the fully merged [Plan 08](08-streaming-and-performance.md) with
its repaired benchmark harness. Then remove verified repeated work in small
changes. Whole-width VT scrolling uses row storage; partial-width scrolling
keeps its cell operations. Parser storage grows within its existing bound.
Periodic recovery work skips only content already acknowledged as durable.
Each change needs its own measured result before the next builds on it.

For users, the intended outcome is less CPU during terminal output and less
private heap and checkpoint churn in quiet sessions. For maintainers, the outcome
is explicit row ownership and local change guards, with the same terminal and
recovery contracts. This plan makes no speedup or RSS-saving promise.

## Evidence and its limits

The original production pin is `9e3f62b56405bdda7a90a32b0acfe3d628fd9208`.
The reviewed benchmark source is `6fc53d6dac7ccc132ffa40bd436749f13b2e2ed1`.
Its production blobs match that baseline; only benchmark tests differ under
`internal/`. Source inspection for this plan uses main at
`0af2b78b62af191919b53846f3372fabd3bdf624`. The measurements below predate
Plan 08's foundations. Later harness integrations do not turn them into after
measurements.

The independently checked evidence is retained in these locations:

- `/work/reports/mesh-plan-08/hotspots.md` contains the original ranking.
- `/work/reports/mesh-plan-08/omarchy-profiles/runtime-final/` contains the
  completed real-worker CPU, allocated-space, and post-GC heap profiles and their
  retained profiling executable. The worker is PID `1896787` in those filenames.
- `/work/reports/mesh-plan-08/omarchy-profiles/` and
  `/work/reports/mesh-plan-08/pi-profiles/` contain microbenchmarks and startup
  traces. Earlier failed runtime directories are diagnostics, not evidence of
  completed workloads.
- `scripts/bench/results/origin-main-9e3f62b/{omarchy,pi}.json` on
  `origin/perf/bench-harness` contains the ordinary-binary results. Each host's
  37 archived median fields recompute from its samples. The independent reviewer
  also checked the raw profile attribution and medians.

The ordinary binaries were hashed and inspected by the reviewer before removal.
Their complete original build/run receipts are missing. The retained runtime
binary does not close that gap for the ordinary runs. Original source, binary,
Go, and target labels were not fully enforced by the harness. Preserve that
provenance caveat; do not reconstruct a receipt from labels alone.

At drafting, [harness PR #62](https://github.com/ShaulLavo/mesh/pull/62) requires
fixes for worker cleanup after daemon death, root disposal, label verification,
and profile completion on write failures. The reviewer verified that the retained
final profiles are readable and completed. That verification supports their
attribution; the original `.done` marker alone does not prove successful writes.

| Original finding | Validated observation | Follow-up decision |
| --- | --- | --- |
| Active VT scrolling | `ultraviolet.Buffer.DeleteLineArea` is 51.90% flat and 62.86% cumulative of 4.20 sampled CPU seconds. Scrollback cell clones are 94.83% of 1,824 MiB sampled allocation. | First residual CPU and allocation candidate. These totals are active-workload allocation, not resident memory. |
| Parser allocation | `x/ansi.Parser.SetDataSize` accounts for about 4,097 KiB live heap; x/vt requests four MiB at construction. | Second candidate, for initial private heap. Worker RSS includes shared mappings. |
| Unchanged recovery | Pi checkpoint benchmark is 1.242 ms, 41,294 bytes, and 273 allocations at a two-second cadence. | Third candidate. Fixture uses a known shell directory and excludes process-directory observation. |
| Unchanged TUI work | Pi catalog apply is 264 µs, 40,742 bytes, and 131 allocations. Inspection apply plus `View` is 5.586 ms, 144,190 bytes, and about 9,600 allocations. | Conditional after watch delivery. Combined apply/render time gives no isolated render attribution. |
| Frame and replay copies | Worker attachment clone is 1.10% of runtime allocation. Warm ring writes allocate zero objects; full raw replay temporarily allocates four MiB. | Defer until residual profiles show a meaningful cost. |

These are rankings, not earned improvements. Three local microbenchmark samples
and one short Pi sample have different confidence. CPU tick quantization, warm
page cache, sampled Go heaps, Unix/loopback transport, and instrumentation affect
interpretation. Pi scratch used tmpfs, so WAL counts do not measure SD-card wear
or fsync latency. Separate ordinary timing from instrumented attribution.

Executable hashing is already addressed by
[PR #59](https://github.com/ShaulLavo/mesh/pull/59); its old Pi initializer cost
was about 677 ms against about 693 ms version startup. Reconciliation writes
are already addressed by [PR #60](https://github.com/ShaulLavo/mesh/pull/60);
the old baseline was about 60 WAL commits per minute. Lean catalogs belong to
[PR #61](https://github.com/ShaulLavo/mesh/pull/61). Do not implement those again.
CLI command construction at 458 µs on Pi and ping at 143 µs per 15-second cadence
do not justify architectural work. Broad lazy CLI dispatch and custom keepalive
are excluded.

## Ownership and dependencies

Mesh's host-owned sessions and detached PTY workers remain unchanged. Connections
stay disposable. Terminal bytes go directly between hosts. The daemon remains
the metadata coordinator, and the Pi remains an ordinary viewer.

| Layer | Current data shape and responsibility | Allowed change |
| --- | --- | --- |
| `internal/worker/worker.go` | `Worker.mu` orders PTY output, ring, screen, attachment, and resize. The pump reuses a 32-KiB read buffer. | Keep output order and lock scope. Never hold the PTY lock over disk or network I/O. |
| `internal/terminal/screen.go` and `recovery.go` | `Screen` hides `vt.Emulator`; `emulatorScreen.mu` protects it. `TextSnapshot` owns bounded row strings. The wrapper tracks a bounded parser tail and metadata separately. | Local screen revision and snapshot tests. Keep the presentation parser and independent snapshots. |
| Pinned ultraviolet and x/vt | `Buffer.Lines` is `[]uv.Line`, each line is `[]uv.Cell`. `RenderBuffer.Touched` tracks dirtiness. `Scrollback.Push` trims and clones cells; `Line` exposes retained storage. | Whole-width movement and an explicit row-transfer contract inside the terminal packages. |
| Pinned x/ansi and x/vt | Parser storage currently conflates allocation size and payload limit. x/vt constructs a fixed four-MiB data buffer. | Separate capacity from the existing logical limit. Preserve dispatch and overflow behavior. |
| `internal/worker/recovery.go` and `internal/recovery/writer.go` | Worker prepares `recovery.Record`; writer owns one pending record and bounded acknowledgement waiters. Digest excludes `CheckpointAt` and advances only after success. | Worker-local unchanged-work guard. Writer stays the durability authority. |
| `internal/tui/catalog_refresh.go` and `inspector.go` | Bubble Tea owns immutable update messages, selection, actions, age, and view state. | Conditional semantic equality and reuse inside the owning model. No second polling cache. |

The inspected pins are x/vt
`v0.0.0-20260828171018-3c30eef5e73e`, ultraviolet
`v0.0.0-20260811164956-006e29f97886`, and x/ansi `v0.11.8` in `go.mod`.
Their source is available in the local Go module cache. Read those exact versions
before changing behavior. Do not modify cached modules.

Submit minimal fixes and regression tests to the owning upstream packages.
Pin a reviewed upstream revision in Mesh through `go.mod` and `go.sum`, then test
Mesh's wrapper against that pin. If an upstream review or usable revision is
unavailable, pause that unit. A permanent local fork, vendor copy, `replace`
shim, unsafe access to private state, or new emulator architecture is outside
this approval. Existing SSH replacements do not authorize terminal replacements.

## Gate 0. Establish the residual baseline

This gate precedes all optimization work. Finish Plan 08, including lean catalogs,
watch, sampler, and subscriber bounds, and merge the repaired harness after its
independent review. Complete Plan 08's own gates before using its result as the
comparison base. Plan 09 does not delay the dashboard's Plan 07 prerequisites
or change that plan.

1. Record the exact merged source commit and clean status. Build in an owned
   scratch directory, and retain a content hash, Go build ID, embedded build
   information, Go version, build flags, and GOOS/GOARCH for each executable.
2. Verify receipts against the executable actually invoked. Record target kernel,
   architecture, CPU, state filesystem, workload, and operation count. Reject
   mismatched labels. Keep raw samples and recomputable medians.
3. Use fixture commands and mock providers only. Own every process and state root;
   prove cleanup after success, interruption, daemon death, and profiling failure.
   Completion requires successful profile write and close, not just a marker.
4. Repeat the original quiet 0/5/20-session and exact-output workloads with ordinary
   binaries. Separately capture the residual active-output profile, quiet-worker
   heap, unchanged checkpoint, and post-watch TUI apply and rendering costs.
5. Rank residual costs and variability. Before each treatment, state whether data
   layout or design is the bottleneck. Keep only units supported by that result.

No run, remote measurement, or installation is authorized by this docs task.
Future local tests use isolated scratch. Any real-host measurement on omarchy,
Mac, or Pi needs separate approval for its isolated run; installed services,
owner sessions, identities, configuration, and state remain untouched.
Real-host installation also needs separate approval. Preserve TV headroom on Pi.
Heavy commands use `bun /work/platform-production/heavy/current/run.js` with the
appropriate `light`, `suite`, `build`, or `bench` class. No bypass or shared-tree
builds. Any development server takes an explicit free `--port`.

## Unit 1. Move whole-width VT rows

The current bottleneck is data layout. `Buffer.DeleteLineArea` visits every cell
to move surviving rows. A whole-width move can rearrange line storage and clear
only vacated rows. Implement the small ultraviolet change first; keep the current
cell path for partial horizontal regions. Preserve vertical margins, clamped
counts, cursor behavior, background blank cells, and `RenderBuffer.Touched`.
Do not assume that a whole-width region covers the whole screen height.

Own the change in upstream `buffer.go` and its line-operation tests. Integrate
only the reviewed pin plus Mesh regression tests in `internal/terminal`.
Do not combine ownership transfer with this unit.

Red/green contracts cover full-width movement without copying surviving cell
arrays; partial-region cells outside the margins unchanged; zero, oversized, and
out-of-region counts; styled blanks and wide-cell placeholders; and touched rows
plus unchanged snapshot/preview behavior. A failing structural or allocation
regression demonstrates the missing fast path; the current semantics tests stay
green. Repeat `BenchmarkScreenWriteLogs` and the ordinary exact-byte throughput
case. Keep the change only if scrolling work falls beyond baseline variability
without regressions in partial-region cost or terminal output.

## Unit 2. Transfer scrollback row ownership

This follows Unit 1. x/vt currently clones outgoing cells before deletion. Removing
that clone requires a real ownership transfer, not retaining a borrowed line.
The screen, retained history, and reusable blank rows must never share mutable
cell storage. Keep `Scrollback.Push`'s copy contract for borrowed callers; use an
explicit owned-row operation for the whole-width deletion path. Audit `Line`,
`Lines`, and `CellAt` readers before reusing any evicted storage.

Own the change in upstream x/vt `screen.go`, `scrollback.go`, and their tests.
If transfer needs an ultraviolet API, deliver and review that small prerequisite
separately. Retain trimming, history order, bounds, main/alternate-screen rules,
and Mesh's 256-line scrollback setting. No general pool or reference counting.

Red/green contracts cover an owned row without a cell clone; retained history
unchanged after later writes and row clears; partial-width scroll with no transfer;
bounded eviction and resize with styled/wide cells; and `SaveText`, `Preview`,
and snapshot continuation equivalence. Run a deterministic scroll workload past
history capacity. Compare allocated bytes per fixed output volume and retained
heap after collection. Require fewer scrollback allocations and stable history
bounds; do not trade allocation churn for unbounded retained rows.

## Unit 3. Grow parser storage within its bound

The current cost is eager allocation. x/vt's constructor calls
`SetDataSize(1024 * 1024 * 4)`. x/ansi's `SetDataSize(0)` enables unlimited growth;
it is not a bounded substitute. Add a minimal reviewed upstream capacity/limit
separation in x/ansi, preserving existing fixed and unlimited caller contracts.
Then enable bounded lazy storage in x/vt as a separate pin-integration change.
Keep the existing four-MiB maximum and overflow dispatch behavior. Do not lower
the limit or change Mesh's `maxParserTail` or restorable-state rules.

The data shape is an owned payload buffer, current used length, and independent
logical maximum. Growth occurs only for collected string data. Reuse bounded
capacity after a sequence; do not add automatic shrinking or per-byte allocation.

Red/green contracts cover low constructor allocation; fragmented OSC with BEL
or ST and DCS with ST; payloads below, at, and above the existing limit;
cancel/replacement/reset followed by a valid sequence; and Mesh title, hyperlink,
parser-tail overflow, and snapshot continuation tests. Retain SOS/PM/APC behavior
where the parser shares these paths. Separate parser API tests from x/vt adoption.
Measure constructor bytes and quiet-worker private live heap, then ordinary RSS
with shared mappings distinguished. Require lower initial allocation, no growth
past the bound, and no material parsing regression for large control payloads.
The old four-MiB cost is an opportunity, not a promised RSS reduction.

## Unit 4. Identify unchanged recovery content

The bottleneck is repeated preparation before the writer's digest guard.
First add a local screen/recovery-content revision under the screen's existing
lock. A conservative revision may advance for every accepted nonempty write and
size change. It must cover scrolling, title, parser-driven content, and resize.
Read the revision consistently with a bounded `TextSnapshot`. Keep this unit in
`internal/terminal/screen.go`, `recovery.go`, and their tests.

The revision is process-local invalidation state, not persisted protocol state.
Red/green contracts cover observing equality without a row copy; output and
scrollback changes; title-only changes; size changes; and independent snapshot
storage during subsequent writes. Existing sanitized recovery text and snapshot
continuation tests remain required. Prove that revision inspection is cheaper
than `SaveText` on unchanged content; do not claim checkpoint savings yet.

## Unit 5. Skip acknowledged unchanged periodic checkpoints

This follows Unit 4. Use one worker-owned checkpoint key containing the screen
revision and all semantic recovery metadata, including directory/source, shell,
command/restart, remote hint, agent recipe/receipt, title, and `LastOutputAt`.
Exclude `CheckpointAt`, as the current digest does. Compare metadata by value;
pointer identity is insufficient. Own any retained record and its slices.

Observe the process directory before evaluating the guard where observation is
currently required. A quiet shell can change directory without PTY output.
Keep observation outside `Worker.mu`, then capture a consistent key and record
inside the current locking order. Reuse bounded prepared text when its revision
is unchanged; skip periodic preparation/submission only for a key confirmed
successfully durable. Prepared, pending, failed, and durable are distinct states.
A queued write cannot authorize a skip. Failure leaves the key eligible for retry.

Keep explicit shell, restart-command, agent, and final-shutdown checkpoints on
the acknowledgement path. A superseding durable record still acknowledges earlier
waiters through the existing writer. Do not acknowledge a failed or pending write
as success. Preserve bounded pending work, failure reporting, and the writer's
content digest. No new per-tick goroutine, global cache, record format, or cadence.

Own the guard in `internal/worker/recovery.go` and narrow writer completion code
only if needed. Split any writer contract change into its own reviewed unit.
Red/green contracts cover periodic no-copy/no-encode after durable success;
directory and metadata-only changes; failed-write retry; pending/coalesced writes
with explicit acknowledgements; and final flush while output or resize races.
Retain `TestCheckpointObservationDoesNotBlockPTYPump`,
`TestShellRecoveryAcceptsOnlyLeaderAndAcknowledgesSavedDirectory`, and both writer
durability tests. Use fixture agent registrations, never live provider accounts.
Measure both known-directory and observed-directory checkpoints. Require less
unchanged preparation/allocation without slowing output or weakening durability.

## Conditional units and exclusions

After the watch baseline, take TUI work only if the remaining cost is material.
Split catalog equality from preview/layout reuse into separate changes. Own each
in `internal/tui` with its model tests. Include all displayed fields, stale/error
state, action reconciliation, selection, summary changes, and geometry in equality
or invalidation. The existing `sessionCatalogChanged` checks only IDs and states
plus staleness; it is insufficient for complete row equality.

Prove unchanged rows avoid `SetItems`/delegate rebuilding, while action completion
and stale/error transitions still update. For preview reuse, prove changed content,
style, geometry, and selection invalidate it. Retain
`TestInspectionOutputAgeAdvancesFromTheLastObservation`, failed-refresh tests,
and action tests. Ages and time-dependent labels keep advancing. Measure apply
and `View` separately, and inspect a fixture-driven terminal at narrow and wide
sizes. Use `verify-fregat` if it supports this Mesh terminal; otherwise record the
control-tool gap and use the repository's terminal tests plus captured output.
Do not suppress all redraws because catalog content is unchanged.

Frame-copy removal and chunked replay remain optional later units. Require new
residual attribution or concurrent-resume memory pressure first. Preserve the
PTY-buffer clone unless ownership actually changes. Any relay change must prove
queue lifetime and disconnect correctness without a pooling scheme. Any replay
change must capture a fixed head, detect overwrite/gaps, copy owned chunks under
brief locks, and keep network I/O outside the ring lock. The ring's zero-allocation
write path stays unchanged. Do not add these units to hide a failed VT result.

## Risks and reversible decisions

Row aliasing can corrupt retained history, and incorrect touched-row tracking can
hide a changed screen. Parser growth can change limit handling or retain a large
buffer after a hostile sequence. An incomplete checkpoint key can lose metadata;
marking a pending key durable can lose retry work. The per-unit contracts target
those failures before performance acceptance.

Keep wire controls, saved record format, history bounds, and resource policy
unchanged. Dependency pins and local guards can be reverted without migrating
saved data. Existing workers keep their PTYs across daemon replacement. A change
that requires killing owner sessions, changing saved formats, or carrying a
permanent dependency fork is a new approval decision, not a step in this plan.

## Verification and delivery

For each implementation PR, capture the narrow failing regression first, apply
one treatment, and rerun that regression plus the existing affected contracts.
Re-run matching ordinary cases and separate instrumented attribution at the
exact candidate commit. Record raw samples, receipts, operation counts, variance,
and measured before/after CPU, allocations, retained heap, and latency as relevant.
Accept a unit only when its claimed benefit exceeds observed variability and
correctness gates pass. A noisy result earns no performance claim. Do not reuse
old extrapolations or idealized component speedups as acceptance thresholds.

Run upstream tests for upstream changes. Mesh code and dependency-pin changes
also require `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`, and
`./scripts/verify.sh`, through the heavy-job wrapper. Exercise actual isolated
worker attach/replay and recovery paths for terminal or checkpoint changes;
unit-only equality is insufficient. Keep process cleanup evidence with each run.
This source-plan PR needs only document/link and whitespace validation.

Apply foundational thinking by resolving row lifetime before removing copies.
Apply the laziness protocol by retaining existing layers and rejecting pools,
forks, and speculative CLI work. Apply sequence-verifiable-units through separate
row, ownership, parser, revision, and checkpoint changes. Apply prove-it-works
through exact binaries and separate ordinary/profile receipts. Read unfamiliar
subsystems with `how`, clean prose with `unslop`, and keep implementation decisions
with `show-me-your-work`. A contested ownership or durability design needs an
independent adversarial review before it proceeds.

Use a dedicated worktree and branch for each unit. Every implementation PR gets
one independent review of its exact head and an exact all-green CI check list
before the coordinator squash-merges it. No commits after approval without
coordinator signoff. The coordinator owns CI, merge order, and any later rollout.
The author hands back scope, measured gates, unresolved prerequisites, and skipped
checks, then stops. A real-host install is a separate decision.
