# Installed fixture clock contract

Status: Approved execution plan, awaiting independent plan review and the
parent's explicit implementation continuation. Final numerical bounds are not
frozen. Managing authorized replacing the single overall signer 600-second
contract in NEXT-DECISION unit 3. This plan grants no runtime run or CI cycle.

Base: C4 `c0c899eea10a9c4f5916b0b629b736455d1d5a92`, PR
[435](https://github.com/ShaulLavo/mesh/pull/435). Keep one continuous signer,
origin and ledger process. Change only fixture execution-clock accounting.
Preserve all product, startup, I/O, observation, soak and coverage contracts.

## 1. Use the retained evidence at its actual boundary

Read `signer-format-fix-plan/REPORT.md`, `NEXT-DECISION.md` and
`17-worker-proof-groups.md` with the actual C4 fixture drivers and exporter.
The reproducible local audit is
`/work/reports/mesh-auto-update/signer-clock-contract/audit.py`. Run it with
Python to reproduce `AUDIT.json`. It reads ZIP members in memory and verifies
archive lengths and SHA-256 against retained API metadata.

| Retained evidence | Actual clock coverage | Missing coverage |
| --- | --- | --- |
| All 24 C3 and 10 C4 ZIPs | 105 `groups/<source>/<profile>/timing.json` records, including 66 in nine successful C3 native shards | No individual native-group start-through-cleanup durations |
| C3 native `timings.json` | 66 complete source-wrapper durations, including child exit and cleanup | Different candidate/context from installed supplemental builds |
| Installed results across both cycles | Ten passed group outcome records with scoped cleanup | No installed-group elapsed duration, including setup and teardown |
| Successful standalone adoption step records | C3 AMD64/ARM64/Darwin wall durations 364/259/391 seconds; C4 370/263/348 seconds | Combined builds, proofs and multiple ungrouped cases, with wall-clock second precision |

C3 caller is `0237728a0673bd9f63b883be57ac5ba004ff78bb`, installed harness
H3 `15aba3473b18df52cbcf53a642a662bc77ca58e3`, preparation
`a0f45b55e31d47ab9f2476de4037c86f8cc2e11d`. C4 uses H4
`bc9b99f98f6bc7b8ae550b6a011f58b137dbe88d`, preparation
`3097dc167bcdcadc8a561fb0f443253b74bfd025`. The format report verifies the
scoped clock/format implementation is identical at these sources. Actual
candidate hashes and selections still differ. Do not pool those contexts as
interchangeable baselines. All nine C4 native shard artifacts are listed in
retained metadata but absent locally. Their success is not a group timing.

For example, C3 artifact 14002044982 contains completed full-proof wrapper
times 206.560223778, 84.348697836 and 79.585554795 seconds for the three
transitions. The exact members, raw values and member hashes are in `AUDIT.json`.
The wrapper starts after context/selection and ends after aggregate validation
and publication. It includes serial native groups and their cleanup, but omits
the enclosing `Releases.prove` setup and final copies. It cannot size individual
groups by division. C3 Darwin waypoint artifact 14003232140 has only two timed
proof wrappers, 299.065338 and 132.006205334 seconds. C4 artifact 14007607701
has 299.797936834 and 126.233318917. No missing third duration is inferred.

The 600-second alarms censor the fixture lifetime, not completed-group clocks.
`signer-lifetime.json` ends before signer stop/reap and may include work after
the exporter already died. Its 600-to-649-second values are not successful
phase samples. C3 cancelled Darwin chain and C4 unconfirmed Darwin oldest
cleanup remain incomplete. `observerSeconds` measures image observation only.
ZIP timestamps, job wall times, failed totals and differences between those
values cannot supply missing group or acquisition/build/publication timings.

## 2. Define every phase before execution

Keep `Releases(root, native, output, *, clock_plan=None)` with its existing
three positional arguments. The parent accepted this shared contract with the
format worker. `clock_plan` is independent of that worker's manifest-capability
selection. Select both before the one `start_signer()` call. Preserve
`check_budget()` and allow an optional phase identity. Keep `close()` idempotent.

Use a small fixture clock helper with frozen `ClockPlan`, `PhaseBudget` and
`SampleRef` records. `ClockPlan` holds context, the ordered phase tuple and
derived `total_ns`. `PhaseBudget` holds its key, positive `limit_ns` and sample
references. `SampleRef` holds source, archive/member digests, measured interval,
outcome and cleanup status. Runtime `origin_ns`, `run_deadline_ns` and active
`phase_deadline_ns` are monotonic instants, never durations or UTC values.
A phase key identifies platform, scenario, source/build
recipe, profile, registry/selection digest, group index and ordered member keys.
Bind actual custom-image digests after their once-only build without changing
the predeclared group count, order or budget. Record harness/preparation source,
exporter capability/overlay, toolchain, runner class and cache conditions.

Use integer nanosecond durations and `time.monotonic_ns()` instants. Never use
UTC for elapsed acceptance. Keep one run origin from entry to
`prove-installed-catchup.py:main` after argument validation, before root creation,
through final scratch cleanup. Pass this run clock through the existing driver
calls; `Releases` uses the same origin. Relative offsets cross subprocess
boundaries as remaining durations, never as new full allowances.

Partition that interval into these adjacent phases in existing source order:

1. Setup: root allocation, noindex/path and native-manager controls, preflight,
   registry staging, constructor work, exporter compilation and empty-ledger
   control. Split at signer launch. Startup: immediately before `Popen` through
   actual control-socket readiness. Start and register teardown before launch.
2. Acquisition: each actual `acquire_pair` plus preceding-version acquisition,
   entry through verified return. Keep ordinary adoption's build-before-acquire
   ordering and chain's acquire-before-build ordering.
3. Build: each complete `Releases.build(version, epoch)` entry through return,
   including all three platform images, archive/hash/version checks, startup
   image and the once-built boundary tool. No excluded build time.
4. Proof: each `Releases.prove(source, target)` entry through return. Subdivide
   into context/selection setup, each `release_worker_groups.run_group` entry
   through verified cleanup and leaf export, and final aggregate validation,
   receipt publication and enclosing copies. Ungrouped order's first proof
   remains one whole proof phase. Retain each existing 600-second grouped or
   180-second ungrouped `run_transition` limit. Propagate fixture group guards
   only from `Releases.run_transition`; standalone native CI keeps its existing
   source-wide 600-second contract.
5. Publication: each `Releases.publish` entry through signed metadata/root write.
   Time custom-source retention, partition freezing, serve-tree/head calls and
   descriptor writes as explicit preparation overhead between these phases.
6. Readers: each waypoint group's entire `verify_readers` entry through its
   three actual executable checks, negative-packet signing and output write.
7. Installed group: before group-directory/case construction through original
   scenario, `finish`/`finish_case`, verified service/process absence, group
   validation and case-root removal. Record body and cleanup subintervals too.
   Ungrouped adoption has one such phase per host/scenario. Order has one whole
   account-owner phase, including all three hosts, original scenario order and
   confirmed driver/account cleanup. Keep its existing 480-second bound.
8. Completion overhead: exact group/matrix aggregation and output, then exporter
   stop/reap, producer-log export, owned root removal and final cleanup records.
   Record signer Popen-to-reap separately from total command duration.

Charge all inter-call and evidence-writing work to explicit adjacent overhead
phases. Reject gaps, overlaps and unfinished intervals at finalization. Child
subintervals explain a parent's duration; never add both to the total. Keep
workflow checkout, tool setup, input acquisition, control steps, upload and tool
cleanup outside this command as separately measured job overhead.

## 3. Measure missing baselines, then freeze numbers

After independent plan review and a parent continuation, implement timing and
mocked failure controls first. This plan authorizes no baseline run. Before a
real measurement stage, the parent must approve its exact source, matrix and
finite calibration envelope. Derive that envelope from the enumerated operation
counts and existing hard operation limits, and record it before launch. Keep
the 900-second hosted job cap. Calibration may end censored and remain blocked.
Do not replace the missing envelope with zero/unlimited time or a guessed total.

Measure all missing phases above on each required native runner class with the
actual original selection and serial scenario. Capture one completed interval
per exact phase/group key, including setup, proof finalization and real cleanup.
Use additional already retained matching complete samples where available.
Calibration uses one signer for that entire invocation and exports partial
evidence on failure. It emits no acceptance/completion marker. Fixes for the
other blockers must already be reviewed; do not retry around missing-session,
format, order or cleanup failures to manufacture a complete timing.

For every phase/group key, choose the maximum eligible completed duration
`M` from the declared sample set. Use integer nanoseconds; for historical JSON
seconds, parse decimal text exactly and round upward to one nanosecond. Derive
`B = ceil((9 * M / 8) / 1 ms) * 1 ms`. This is 1.125 times the measured maximum
plus less than one millisecond. Reject absent, failed, censored or mismatched
samples. Never average across groups, platforms or profiles. Keep the sample
references and the arithmetic beside every bound.

For illustration only, the three C3 AMD64 chain wrapper samples above derive
232.381, 94.893 and 89.534 seconds, totaling 416.808 seconds. This is only a
wrapper subtotal. It omits builds, setup, installed groups and terminal cleanup,
and is not an executable whole-run bound.

The fixture total is the sum of the disjoint leaf budgets, including actual
setup and cleanup. A parent proof guard may use a separately measured whole
proof budget; it is an additional guard and is not added again. The job total
adds measured external workflow overhead. Freeze these numbers and their full
expected key sequence in reviewed source before a separate acceptance run.
No live measurement may resize the running plan, write its own passing bound,
borrow unused phase allowance or extend an absolute deadline. Missing baseline
means freeze blocked. If the derived full job cannot fit 900 seconds, stop for
an explicit execution-shape decision. This plan does not extend jobs or shard
an invocation into new signer lifetimes.

## 4. Enforce one lifetime and preserve the claims

Set the run deadline once to origin plus the derived fixture total. Set each
phase/group deadline once at entry using its frozen budget. Enforce the earlier
deadline and every existing inner operation limit, before and after each call.
Cap blocking subprocess/socket waits by remaining time. Use one monotonic
watchdog for in-process work to enter the existing interruption/cleanup path.
Native group subprocesses retain process-group ownership and bounded teardown.
The Go exporter gets `-test.timeout=<remaining run duration>ns` at its sole
launch. Never pause that process clock. Remove both hard-coded signer `600`
checks together; preserve native source-wide and per-transition 600 limits.

On timeout, exporter death, malformed reply, assertion or cleanup failure,
record the original failure, stop new work and run existing bounded teardown.
Cleanup after expiry cannot turn the run green. Retain explicit unconfirmed
resources if teardown fails. Emit success only after exact coverage, every
clock guard and terminal cleanup pass. No relaunch, trust reset, re-signing,
stale-ledger fallback, or additional fixture identity may rescue that invocation.

The single Go exporter owns its private keys, Sigstore/TUF proxy, TLS origin,
control socket, signing, seed, inspect and validated ledger for the whole run.
Python owns its process and immutable input/output inventory. Keep its existing
one-hour TLS/TUF validity; reject a plan that outlives that material. Stop/reap
the exporter after all case consumers stop, then remove only owned material.
Keep pre-existing caches/services untouched. A group owns its fresh state,
original attached worker, selected workers/shells, daemon/helper/managers,
terminal/listener/route observer and case root. Confirm their absence before
the next group. The fixture signer is the expressly shared dependency.

Keep the exact ten cohort jobs: oldest, chain and waypoint on linux/amd64,
linux/arm64 and darwin/arm64, plus order on linux/amd64. Oldest repeats adopt
and rollback for every selected group. Chain and waypoint repeat none, first
and final failure scenarios for every group; waypoint also runs all original
reader checks. Preserve standalone adoption's five scenarios per inventory host
and the interrupt-only companion selection, all three fresh-managed jobs and
the original schema 1/native proof matrix. Never drop a historical member,
deduplicate a protocol or change the 31-worker/1-GiB partition and custom-source
rules to fit a bound.

Preserve all exact group/member/label validation, PID/image/protocol/token and
original attachment identity, hop/helper/rollback/quarantine, failed-edge restart,
route/preservation and independent-state claims in plan 17. Keep 5-second
observations and both order/soak windows, eight hashing workers, 128-MiB image
bound, 6-second health, 30/90-second subprocess, 150-second chain, 90-second
rollback and 45-second quarantine limits. Groups prove independent runs, with
identity continuity within a group only. They do not prove cross-group overlap.

Keep release signatures and bytes immutable after signing. Actual supplemental
signatures stay tied to their original exporter/trust identity. Keep the old
waypoint executable rejecting actual schema 2 bytes and the future-capable
exporter's fully validated ledger selected by the parallel format correction.
Fixture success cannot replace main-only signed-draft validation. Preserve the
signed native index, complete installed matrix, exact subject/digest checks and
post-sign completion gate. Keep `firstPublishedUpdater: null` conditional.

## 5. Review and stop at the authorized gate

The parent commissions independent review of this plan, applies bounded review
fixes and explicitly resumes this same unit before implementation. No delegate
or implementation starts in this plan phase. Future mocked controls cover
rounding, exact key accounting, no self-sizing, monotonic wall-clock immunity,
one launch, early/late phase expiry, signer death, partial output and cleanup
refusal. Verify both Python and Go use the frozen total, without weakening any
inner window. Normal required checks and independent source review follow.

Any corrected harness needs an immutable H checkpoint and the consistent five
caller-reference refresh. Cycle 5 requires reviewed NEXT-DECISION units 1-4,
complete frozen measurements and separate existing authorization. This plan
supplies no CI permission, seed/build/dispatch/retry, source publication, PR
push/watch/merge, fleet/helper/trust/proxy/kernel change or paid runner.

This planning unit retains only the local plan commit and identical report,
plus its small reproducible audit. No runtime resource was created, so no runtime
cleanup is needed. The T3-provided exclusive tree stays parent-owned at the
C4-based plan commit. Parent owns these report artifacts until review and lane
closeout. Implementation has not started.
