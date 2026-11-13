# Measured installed clocks

Status: Approved for the exact 36 indexed operation-span workloads after
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
censored, failed and instrumented records do not size any active plan.

The external observation is the successful terminal provider job duration plus
two seconds for endpoint precision, minus that VM's monotonic command duration.
Approval assumes ordinary UTC/monotonic elapsed-rate comparability with no
material clock step. Raw external_complete remains false. No unmeasured segment
or monotonic bridge is invented.

Source/group 600-second, ungrouped 180-second and order 480-second limits, all
product/I/O checks, five-second observations and soaks, finite cleanup reserves
and the 30-minute job ceiling are unchanged. Runtime span deadlines never reset
on nested operations or resumed leaves.

Only linux-arm64-chain-consume-none remains unindexed pending a completed clean
measurement and independent review. The original failed records, including the
frozen smoke setup overrun, remain failures. Publishing a new authoritative
worker registry requires matching recalibration; old sample workloads are never
relabeled to admit a newly published worker.
