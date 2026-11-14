# Measured installed clocks

Status: Approved for the exact 37 indexed operation-span workloads after
independent numerical and code reviews on November 13–14, 2026. Numerical
eligibility does not establish successful frozen or native acceptance.

The original 33-plan packet reuses completed raw records from runs 43421606579,
43451015125, 43462713405 and 43500072280, retaining each source, run and attempt.
Its 1,675 leaves partition into 450 canonical operation spans. All 201 guard
objects, external observations, reserves and admissions are unchanged. Every
command sum is no larger than its original leaf-plan sum. The independent review
is retained at frozen-budget-smoke/SPAN-NUMERIC-REVIEW.md under the lane reports.
The original packet SHA-256 is
501ae90d075df654236d006793445b5c911b401599c49cb2e40dd766fc2c4d7d.

Three additional complete consumers come from run 43504503722, attempt 1,
harness 4c6452f3. AMD64 chain-final, ARM64 waypoint-final and Darwin chain-none
each retain 53 raw leaves, nine spans and five full group guards. Their total
command, external and 60-second reserve allocations are respectively 659.629,
704.629 and 540.378 seconds. Independent review approved 900-second admissions
and 15-minute provider timeouts for all three. Original diagnostic metadata
remains at 1,800 seconds. The review is retained at
signer-clock-contract/split-budget-reducer/consumer-43504503722/span-candidates/REVIEW.md.

Every span and guard bound is its completed same-invocation measurement
multiplied by 1.125 and rounded up to milliseconds. Exact canonical membership,
source hashes and leaf endpoints are retained. Only disjoint spans enter the
command sum; nested guards remain independently enforced. Incomplete, unclean,
censored, failed and instrumented records do not size command plans. The narrow
external exception is described below.

The external observation is the terminal provider job duration plus
two seconds for endpoint precision, minus that VM's monotonic command duration.
Approval assumes ordinary UTC/monotonic elapsed-rate comparability with no
material clock step. Raw external_complete remains false. No unmeasured segment
or monotonic bridge is invented.

Source/group 600-second, ungrouped 180-second and order 480-second limits, all
product/I/O checks, five-second observations and soaks, finite cleanup reserves
and the 30-minute job ceiling are unchanged. Runtime span deadlines never reset
on nested operations or resumed leaves.

The last ARM64 chain-none consumer was independently approved from successful
run 43524814720, attempt 1, actual harness e94fb263. Its 53 completed leaves form
nine spans with five separate guards. Command 301.788, external 70.592 and reserve
60 total 432.380 seconds, within its 900-second admission and 15-minute timeout.
The review is retained at split-budget-reducer/arm64-last-43524814720/REVIEW.md.
All 37 workload slots are indexed. The original failed records, including the
frozen smoke setup and signer-build overruns, remain failures. Publishing a new authoritative
worker registry requires matching recalibration; old sample workloads are never
relabeled to admit a newly published worker.

The 19 bootstrap and preparation producer drafts were independently reviewed at
55b0f8bd. Their packet SHA-256 is
63f88e3189a67985271734e146c7b625888e9712cd448c1e87e72b82d7136743.
For Linux ARM64 adoption interrupt only, an explicit owner exception approves
the external interval from terminal failed job 132309161661. Its command and
acquisition remain incomplete, and its raw external record remains incomplete.
The reviewed external overlay SHA-256 is
a89f8f16b834f633a7314dbe7083f4b718051ecedc7740acd7fdfc2a3fb5eb29.
The future successful cache-save tail remains unmeasured.

Darwin fresh also retains both complete invocations from the original sample
and successful population job 132322120211, run 43553838184, source 55b0f8bd.
Every phase and telemetry row uses its larger completed measurement at 1.125,
with both full source streams preserved. The independently reviewed candidate
SHA-256 is e323521d16cd8155a70a9d3a64506ef9e696e3dbd19b4660661132731ba08a42.
Command, external and reserve total 500.828 seconds within the unchanged
900-second admission. All independent guards remain unchanged; diagnostic
acceptance stays false. See acceptance-ae2a/darwin-fresh-candidate/REVIEW.md in
the retained lane reports.
