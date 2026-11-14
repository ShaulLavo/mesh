# Installed fixture clock contract

Status: Approved execution plan with parent-applied bounded corrections from
PLAN-REVIEW.md. Implementation may continue under the parent's instruction.
Numerical freeze and acceptance remain held. Managing authorized one
measurement-only hosted run of the full grouped cohort, within current overall
and operation bounds and the existing 900-second job admission.

Base: C4 c0c899eea10a9c4f5916b0b629b736455d1d5a92, PR 435.
Keep one continuous Releases signer/origin/ledger owner, with no restart,
clock pause, trust reset or re-signing rescue. Only the explicitly authorized
fixture-clock contract changes. Product, I/O, observation and soak limits stay.

## Evidence and eligibility

The retained audit reads all 24 C3 and 10 C4 archives and verifies their API
lengths and SHA-256. It finds 105 completed whole-grouped-proof records and
66 outer native-source timings, but no complete individual native-group or
installed-group entry-through-cleanup samples. Ten passed installed outcomes
are not elapsed measurements. Do not divide wrapper totals, subtract ZIP/job
times, use observation-only intervals or treat 600-second alarms as completed
samples. The original audit and source mapping remain in signer-clock-contract.

Separate immutable provenance from budget eligibility:
- Provenance records actual harness/preparation IDs, executable and overlay
  hashes, archive/member hashes, toolchain, runner/cache facts and output identity.
- The workload contract identifies operation kind and occurrence, platform and
  runner class, scenario/profile, source/target recipe and epoch/state range,
  immutable published members, ordered custom roles and exact partition.
- Before acceptance, independently review the mapping from calibration provenance
  to that source's unchanged workload contract, retaining the frozen bounds.
  A new Git commit or budget-file edit alone neither invalidates that mapping
  nor raises a limit. Changed work or context needs explicit eligibility review
  and recalibration when the sample no longer applies.
- Resolve custom hashes and sizes once after build, validate their roles and
  partition and bind them to the result. They never select new limits.
  Publication's exact signed-byte checks are unaffected.

Use only a small ClockPlan, PhaseBudget and SampleRef helper, not a scheduler,
measurement service, signing protocol or context-equivalence framework.
ClockPlan freezes context, ordered keys and derived total. PhaseBudget holds
a positive duration and eligible sample references. SampleRef holds exact
provenance, the measured boundary, functional outcome and required cleanup.

## Accounting boundaries

Keep Releases(root, native, output, *, clock_plan=None), with the existing
three positional arguments. Manifest capability is a separate keyword selected
before the one start_signer call. Keep check_budget compatible with an optional
phase identity and close idempotent. A constructor may remain deferred for pure
tests, but no executable acceptance call may omit a plan and silently disable
clocks or obtain a fresh 600-second allowance. Migrate every caller, including
standalone adoption and fresh-managed.

Use integer nanoseconds and monotonic_ns. Instants and durations are distinct.
One command origin begins after argument validation, before root allocation,
and ends only after final owned-root cleanup and result writing.

The following are operation types in actual call order, not eight global
phases. Include operation kind, version/epoch or source/target, and occurrence
so repeated calls cannot collide. Close one leaf and open the next at one
monotonic timestamp:
1. Setup/startup: root/path/noindex/manager controls, registry staging,
   constructor work and exporter compilation; split immediately before Popen,
   then time startup through actual control-socket readiness.
2. Acquisition: each exact pair/predecessor acquisition through verified return.
   Preserve each scenario's actual acquire/build ordering.
3. Build: complete build(version, epoch), all three platform images, archive/hash/
   version checks, startup image and once-built boundary tool.
4. Proof: complete Releases.prove setup, selection, every native run_group from
   entry through verified cleanup/leaf export, then aggregate validation,
   canonical receipt publication and enclosing copies. Record the ungrouped
   order proof as its existing whole phase.
5. Publication/preparation overhead: signing, metadata/root writes, custom-source
   retention, partition freezing, serving/head and descriptor writes.
6. Readers: each waypoint group's complete executable checks, negative-packet
   signing and output, preserving the actual original reader rejection.
7. Installed group: before directory/case construction through the original body,
   finish, verified service/process absence, group validation and root removal.
   Standalone cases have their own leaves. Order owns its existing complete
   three-account scenario and verified cleanup.
8. Completion: group/matrix aggregation, exporter stop/reap, producer-log and
   result export, noindex-after and final scratch-cleanup records.

Charge all inter-call work to named adjacent overhead leaves. Reject gaps,
overlaps and unfinished success intervals. Nested body/cleanup and whole-proof
intervals may explain or additionally guard parents; do not sum them twice.
Signer Popen-to-reap is a separate explanatory interval. Workflow checkout,
tools, input transfer, upload and tool cleanup are separately measured external
job overhead, not omitted work.

## Calibration and independent numerical freeze

Calibration is an explicit diagnostic mode with a predeclared finite envelope.
It is not acceptance against missing or same-run-derived PhaseBudget values.
Managing pre-authorized one full grouped-cohort hosted workflow. Passive Linux
and order evidence may join only if their code reviews pass in time.

Before dispatch, parent verifies exact source/capability, full required matrix
and ordered keys, operation counts, context, current overall/operation bounds,
finite envelope, external overhead, emergency cleanup/export reserve and owner.
Keep 900-second admission and check live remaining job time before command
launch. The sum of operation maxima is not proof of a fit: three existing
grouped transition allowances already sum to 1,800 seconds. Taking the minimum
with available time merely bounds a possibly censored diagnostic. It proves
neither impossibility nor complete admission. No unlimited/zero envelope,
larger job, owner restart, retry-through-failure or hidden job split is allowed.

Record completed local boundaries even when a later diagnostic stage fails,
provided the local work, outputs and required cleanup are valid. A partial
interval or unconfirmed cleanup is ineligible. A local sample never establishes
whole-matrix or whole-run success. All missing keys remain explicit and block
freeze; do not drop groups or pretend censored time covers their full work.

For each exact eligible key, use the maximum declared completed interval M.
Parse historical decimal text exactly if it is eligible; never reserialize a
binary float as measurement authority. Compute:
B = ceil((9 * M / 8) / 1 millisecond) * 1 millisecond.
Thus bounds have 12.5 percent headroom plus less than a millisecond. Do not pool
different groups/platforms/profiles or resize a running plan.

The command total is the sum of disjoint successful-work leaf budgets,
including setup and cleanup. Independently frozen external-overhead budgets
use the same rounding/headroom policy; raw observed overhead is not itself a
future upper bound. Add a separately declared finite emergency failure-cleanup
and export reserve to job admission, without omitting or double-counting normal
cleanup. That reserve grants no green execution time.

Commit independently reviewed numeric values, full key coverage, provenance
mapping, arithmetic and admitted totals before a separate acceptance run.
If calibration censors, or a complete group/derived invocation cannot fit
admission, stop with its actual interval, missing keys and a proposed job split
for managing. Do not enlarge the job or alter execution shape silently.

## Deadline handoff and failure ownership

Set the absolute command deadline once. Set each frozen phase deadline once
at entry; enforce the earlier and every unchanged inner limit. Parent deadlines
remain authoritative across dispatch, launch, IPC and return. A transmitted
remaining duration cannot restart after dispatch latency. Apply this to native
group, ordered owner/driver and origin/socket waits.

Releases.run_transition supplies fixed ordered group limits and remaining
enclosing allowance to the grouped child. That child enforces entry through
verified cleanup and leaf export; standalone native execution retains its
existing source-wide bounds. Keep grouped/source proof 600 seconds, ungrouped
transition 180, ordered owner 480, and all current product startup, I/O, health,
hashing, settlement, quarantine, five-second observations and both soak windows.

Do not assume current teardown is wholly bounded: a wrapper leader can exit
before descendants, and ordered recovery currently lacks an outer timeout.
Add the small explicit handoff and finite recovery/cleanup allowance. Latch the
first failure, stop new work and disarm repeated watchdog interruption before
teardown. Gracefully stop, then bound escalation/reaping and verify owned
descendant/service absence even after leader exit. Never remove a consumed root.
Preserve the original error, cleanup errors, incomplete timings and retained
resource identities. Any emergency allowance fits the job admission and cannot
turn an expired case green. Terminal success follows final cleanup and the
last applicable guard.

Give the single Go exporter its reviewed remaining lifetime at its sole launch;
do not pause/relaunch it. Keep one-hour TLS/TUF material validity and reject an
outliving plan. Stop/reap after consumers end; remove only owned material and
preserve shared caches/services.

## Validation and preserved claims

Keep the ten cohort jobs (oldest/chain/waypoint on all three platforms plus
Linux AMD64 order), every selected worker and source/profile, 31-worker/1-GiB
partition, exact custom roles and all standalone/fresh/native/schema-1 checks.
Each group owns a fresh state, original attachment, selected workers/shells,
daemon/helper/managers, terminal/listener/route and case root. Cleanup precedes
the next group; identity continuity is within that group only.

Keep all original label/member/PID/image/protocol/token/attachment, hop/helper/
rollback/quarantine, failed-edge restart, route/preservation and signed-byte
assertions. The future-capable fixture ledger remains explicit and independent
of the old reader, which still rejects real schema-2 bytes. Private fixture
success cannot replace main-only signed-draft validation. Preserve complete
native/index/installed/post-sign completion gates and firstPublishedUpdater null.

Focused deterministic controls cover exact rounding/keys, missing/censored
samples, no self-sizing, wall-clock immunity, one launch, phase expiry, exporter
death, malformed/empty/stale replies, leader exit before descendants, delayed
handoff, timeout in cleanup, ordered recovery exhaustion and partial timing
export. No generic supervisor rewrite is needed. Independent code review
precedes calibration; independent numerical review precedes acceptance.

Parent applies these bounded plan corrections and explicitly resumes the same
author for timing/mock implementation. No implementation ran in the plan phase.
Cycle 5 requires units 1 through 4 in/reviewed, complete numerical freeze and
existing managing authorization. This plan grants no other CI cycle, case,
source publication, merge, fleet/helper/trust/proxy/kernel change or paid runner.
