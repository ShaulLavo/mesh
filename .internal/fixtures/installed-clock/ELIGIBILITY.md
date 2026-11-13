# Measured installed clocks

Status: Approved for the exact workloads in these plans after independent GPT
numerical review on 2026-11-13. Measurements come from hosted run
[43421606579](https://github.com/ShaulLavo/mesh/actions/runs/43421606579), source
`a5cdb054ab73c6a9cf6fbb0a85f4b34f68fd1676`.

Every phase and group bound is the measured elapsed nanoseconds multiplied by
1.125 and rounded up to milliseconds. Only disjoint phase bounds enter the
command sum. Nested group guards are checked separately. All three oldest
workloads completed with verified scoped cleanup and complete group coverage.
Linux includes passive catalog evidence; Darwin does not.

The external observation is the terminal provider job duration plus two seconds
for whole-second endpoint precision, minus the measured monotonic command
duration. Its bound uses the same 1.125 rule. The review checked completed upload,
post actions, cache hits with no save, and timestamp consistency. This is an
explicit aggregate upper observation under ordinary UTC/monotonic elapsed-rate
comparability. Raw `external_complete: false` remains false; no missing external
segments or monotonic bridge are manufactured.

Linux AMD64 and ARM64 keep 900-second admissions. Darwin admits 1175 seconds and
needs a 20-minute provider timeout. Command, external and 60-second emergency
reserve total 746.297, 705.794 and 1174.913 seconds respectively. The 30-minute
ceiling and all source, group, product, I/O and soak limits remain unchanged.

The seven incomplete workloads have no activated plan. Prepared producers,
consumers, standalone adoption and fresh starts need their own measured plans.
The reducer and full review are retained under
`/work/reports/mesh-auto-update/signer-clock-contract/numerical-freeze/`.

Four Linux support plans were independently approved from run
[43451015125](https://github.com/ShaulLavo/mesh/actions/runs/43451015125), harness
`7f272dce9c70289f95d1111e8fb7c55ba4de9845`. AMD64 and ARM64 fresh and explicit
interrupt adoption have 146 complete disjoint leaves with verified cleanup.
Their command, external and reserve totals are 277.148, 304.148, 340.158 and
318.771 seconds respectively. All keep 900-second admissions and 15-minute
provider timeouts. The same 1.125 arithmetic and explicit aggregate external
method apply. Raw external completeness flags remain false. These plans do not
cover adoption-all, Darwin support or prepared producer/consumer workloads.
Their review and raw evidence references are under
`/work/reports/mesh-auto-update/signer-clock-contract/split-budget-reducer/`.

Snapshot 2 from the same run independently approved seven more workloads:
Linux AMD64/ARM64 adoption-all, Darwin interrupt adoption, and all four Linux
chain/waypoint preparations. These use 900-second admissions and 15-minute
provider timeouts, with exact derived totals of 559.552, 408.782, 481.893,
865.535, 882.409, 811.530 and 784.534 seconds respectively. Their clocks,
groups, descriptor identities, cleanup and terminal provider overhead were
checked independently. Existing oldest and support plans remain unchanged.
The review is in `split-budget-reducer/snapshot-2/REVIEW.md` in that report tree.

Darwin fresh was independently approved from the final retained packet of run
43451015125. Its command, aggregate external and reserve total 463.898 seconds,
with a 900-second admission. The same arithmetic, cleanup and provider-boundary
conditions apply. Its review is `split-budget-reducer/terminal/DARWIN-FRESH-REVIEW.md`.

The final recovery review approved seventeen more exact sidecars: fourteen
complete-profile consumers and both Darwin preparations from run 43462713405,
plus Darwin adoption-all from its separate successful retry in run 43451015125.
Their source/run/attempt packets remain separate. Existing fifteen plans are
unchanged. Most admissions are 900 seconds; Darwin waypoint final uses 1020,
Darwin chain preparation 1200, and Darwin waypoint preparation 1500. Every total
is derived from complete clean measurements with the same 1.125 rule and explicit
aggregate external conditions. The review is `split-budget-reducer/recovery-final/REVIEW.md`.

The ordered-host workload was independently approved from successful run
43500072280, attempt 1, measured harness `ba56cc36`, distinct from vehicle
`e60c2536`. Its 43 clean leaves and eleven guards give a 490.186-second command,
138.719-second aggregate external bound and unchanged 60-second reserve,
totaling 688.905 seconds within a 900-second admission. The full successful
provider interval includes post actions and cache work; raw external completeness
remains false. Its review is `split-budget-reducer/order-43500072280/REVIEW.md`.

Four slots remain unactivated: Linux AMD64 chain final, Linux ARM64 chain none,
Linux ARM64 waypoint final, and Darwin chain none. Their failed or missing
records provide no successful command plan. These numerical approvals do not
establish successful frozen execution; the separate frozen smoke setup overrun
remains recorded and under phase-contract review.
