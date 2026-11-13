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
