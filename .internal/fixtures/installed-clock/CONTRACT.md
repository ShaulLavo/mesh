# Installed frozen plan selection

The 33 indexed numeric files are local operation-span drafts. Their empty
`eligibility_review` deliberately blocks execution. Fresh independent numerical
and code reviews must approve the new grouping before the parent activates it.
Historical leaf-plan approvals do not approve span semantics. Index selections,
external observations, admissions, reserves and all 201 guard objects are unchanged.

Each phase key is `span:<first raw leaf key>`. Its workload is the invocation
workload plus `clockSpan: {leaves: [...]}`. The strict pure grammar in
`scripts/fixtures/installed_spans.py` binds operation identities, nested resumes,
heads, complete group indices and readers to the retained workload. Setup joins
its following whole operation. Consecutive declared release/public serving and
heads share one span. The exact final setup/completion pair has its own span.
Unknown shapes fail before execution. Old leaf plans have no loader fallback.

A span deadline starts at its first leaf and survives all nested calls and
resumes. Command, active span, stacked guards and parent handoffs remain absolute
bounds. Terminal export stays in the final span through the final write/check;
late returns are censored and cannot accept. Raw leaves and operation samples
remain diagnostic evidence.

Run `python3 -B scripts/migrate-installed-spans.py --output OUTPUT` to rebuild
the exact candidate packet from the retained same-invocation sources at the
historical baseline. `--write-drafts` also replaces this exclusive tree's numeric
files, leaving eligibility empty. The tool verifies original samples with their
measured harness API and validates draft shape with the current API under an
explicit test-only marker. No review marker is emitted into runtime plans.

The parent owns `index.json` and plan files. After independent approval, the index
has this shape, with paths relative to this directory:

```json
{"schema": 1, "plans": {"linux-amd64-oldest": {"path": "linux-amd64-oldest.json"}}}
```

Each file must load as a reviewed frozen `ClockPlan`. Selection keys are
`<platform>-<profile>`, where platforms are `linux-amd64`, `linux-arm64` and
`darwin-arm64`. Cohort profiles are `oldest`, `chain`, `waypoint` and Linux AMD64
`order`. Linux oldest with catalog evidence uses `oldest-catalog`. Support
profiles are `fresh` and `adoption-all`, or `adoption-<scenario>` for an explicit
`adopt`, `rollback`, `helper`, `initial` or `interrupt` selection.

Ordinary Linux oldest jobs retain passive catalog evidence and select
`oldest-catalog`. Darwin oldest selects `oldest`. Calibration preserves its
explicit catalog input. A false catalog workload cannot use a catalog plan.

The selected plan must match the exact invocation workload, including host,
registry digest, scenario and catalog flag. A grouped plan cannot cover a
standalone adoption profile or fresh run. The workflow selects budgets before
launching those jobs, rounds each reviewed admission up to minutes, and refuses
anything beyond 30 minutes. Each consumer records its same-host monotonic job
origin before checkout and setup with exclusive creation. It never resets that
origin when the fixture starts. Calibration retains its explicit diagnostic
envelope and does not read or populate this index.

Support measurement is selected explicitly with `calibration_only: true` and
`calibration_support: adoption-all`, `adoption-interrupt` or `fresh`. Each runs
the three native platforms under the original 1,800-second diagnostic job clock
and 1,500-second command envelope, with 240 seconds external and 60 seconds
emergency reserve. The default empty selector retains the ten-cell calibration
cohort. Diagnostic completion never means frozen acceptance.

Prepared producer/consumer CLI forwarding is available through
`--export-prepared PATH` or `--prepared-releases PATH --installed-slice JSON`,
with explicit chain or waypoint mode. Frozen prepared roles require an explicit
reviewed `--clock-plan` carrying the matching `preparedRole`. They cannot select
an unsplit logical-cell plan from this index. The parent composes physical-slice
index/workflow mappings after reviewing the prepared workload contract and
budgets. The frozen workload retains `preparedRole` and canonical
`installedSlice`. The exact `preparedId` is the SHA-256 of the actual newly
signed `prepared.json`. It belongs in execution provenance and same-bundle
aggregation, since normal signing timestamps change it on each producer run.
It does not select or invalidate numerical budgets. Independent review must
verify that removing this identity from budget matching preserves exact bundle
validation and same-bundle equality. This wiring does not infer a slice or
invent a budget.

The publication draft path is separate in the supplied source. It needs its own
reviewed workload and complete measurement coverage; these selectors do not
claim that grouped calibration covers real signed publication.
