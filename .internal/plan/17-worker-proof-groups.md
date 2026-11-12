# Historical worker proof groups

Status: Approved. Managing chose options 1, 2 and 5 on 2026-11-12. This changes
historical proof concurrency only. No paid runner, deadline increase, coverage
reduction, runtime optimization, fleet change or reopened Fable round.

## Baseline and claim

Implement on `66b17b3b2eca2695b495be79bf0785973e3d5128`, the PR 435 integration
`d789fcee7c18e37a0dc8fabfc4f4c69d21c8fcea` plus independently approved
`44069317ecc0dea5acf6b0965c845af0b571f5b7`. Parent is absorbing that fix into
435. Keep PR 459's `307dcb9fd101ac6e3dbfa471848c6a155acc2159` startup observer
and partial-cohort export after its independent review. It diagnoses Linux
EPERM and does not fix it. PR 458's listener diagnostics/root fixes stay separate.

Before: `run-update-proofs.py` shards sources, then runs one
`prove-release-transition.sh` per source. `release_workers.applicable` selects
every registry version at or below the source, hashes every executable, and
adds the actual source image if absent. All selected workers coexist with an
additional original attached source worker in one state. Their exact identities
survive the whole forward-only or full profile. Full profiles include every
helper rollback boundary and actual candidate startup fault. Installed chain
and waypoint cases similarly retain one simultaneous cohort through their hops.

After: partition that exact selection into consecutive groups. Each group gets
a fresh original source state, original attached worker, saved recovery fixture
and the entire original profile. Every member survives all boundaries required
by that profile. Identities persist within that actual state only. The result
certifies complete historical coverage across independent runs; it no longer
certifies that workers in different groups survived concurrently. Runtime
health/readiness rules and the real product's support for arbitrary live
sessions do not change. This is the managing-approved tradeoff.

## Execution shape

1. Keep anchor/window/source selection and `selectWorkerRegistry` in `mesh-release-routes` and
   `prepare-release-baselines.py` unchanged. Freeze `applicable` once per source,
   preserving registry order and member identity, including version, protocol,
   platform and archive/executable hashes. No protocol deduplication or dropped
   entries. Partition deterministically, at most 31 selected workers per group,
   and at most 1 GiB of executable bytes including the original attached source
   image. Fail if one member plus that image cannot fit. The existing 4096-member
   selection bound stays. A source image already in the registry still has its
   selected worker as well as the separate original attached worker.
2. Add a small `fixtures/release_worker_groups.py` helper. The outer
   `prove-release-transition.sh` invokes the existing single-group body through
   an explicit validated group descriptor. Run serially, with no next group
   until processes and state from the previous group are confirmed absent.
   Reuse prepared candidate/probe/boundary/startup binaries and archive caches.
   Do not rebuild tools or reacquire history per group. One source's existing
   600-second deadline covers all its groups, rather than resetting per group.
3. `release_workers.start` receives the validated selected subset. Keep
   reservation, ownership-before-readiness, original process parentage and PR
   459's failure observer/export. Keep `verify_images`, full executable hashes,
   peer credentials, PID/shell/protocol/build checks and token I/O. Export a
   real observation per required label and original attached-worker identity.
   Validate the exact expected labels, not the present `len >= 3` shortcut.

At most 32 retained workers and 32 shells coexist per source/profile, plus the
existing bounded daemon/helper/client processes. Active images total at most
1 GiB, compared with the measured 3.36 GB at 122 workers. This is a conservative
count/image working-set bound, not an invented RSS ceiling. Record actual peak
RSS and memory pressure in the next hosted cycle; unrelated OS memory and the
shared file cache are outside it. Do not purge caches or claim a measured gain.

## Evidence and publication

Keep schema 1 manifests, forward receipts and rollback proof fields unchanged.
Add a companion format, never extra fields to strict legacy documents:

```text
partition(selected, originalImageBytes) -> Group[]
run_group(context, Group, remainingSourceDeadline) -> GroupEvidence
validate(context, selection, GroupEvidence[]) -> VerifiedGroups
finalize(VerifiedGroups) -> actualForwardReceipt, derivedRollback?, companion
Group = {index, members: MemberKey[], imageBytes}
MemberKey = {version, commit?, protocol, platform, archiveSha256?, binarySha256}
GroupEvidence = {context, group, stateRun, observationsRef, forwardRef,
                rollbackRef?, cleanupRef}  // PIDs exist only in observations
context = {harnessSource, preparationSource, registrySha256, sourceDigest,
           candidateDigest, platform, profile, firstState, lastState}
```

Per-source `<sha>.worker-groups.json` binds the partition and hashes of actual
per-group receipts, observations, startup-image context and cleanup records.
Use relative content-addressed references with existing path confinement.
Documents are bounded to 1 MiB each; the complete selected set remains bounded
to 4096. Store leaves separately, never flatten PIDs into a combined cohort.
`mesh-worker-groups.json`, a bounded index of source/platform/profile, canonical
receipt references and companion digests, becomes an additional signed subject.
No changes to host wire protocols or installed reader schemas are needed.

Validation fails closed on missing, duplicate, extra, failed or interrupted
groups, members, profiles or labels; mismatched source/candidate/platform; wrong
image hashes; changed registry; or unconfirmed cleanup. Compare the partition's
exact member multiset with fresh authoritative selection. Check each group's
own start-to-end identities and every required boundary against its actual
records, including startup companion identity. Never merge identities across
states. No successful completion marker is written after partial failure.

All groups must produce byte-identical forward receipt bytes. Retain one actual
receipt as canonical. Derive the legacy rollback digest set as the exact union
of validated per-group worker digests plus the original source, and each boundary
flag as the conjunction of that boundary's verified group results. Reject any
disagreement in other fields. This digest-set summary has no concurrency or PID
claim; the signed companion explicitly records independent groups. Keep original
per-group rollback assets so every summary assertion can be checked. Never emit
booleans from a generated boundary range without matching observed labels.

Update `collect-update-proofs.py` to validate all groups before exposing canonical
assets to `assemble-release-compatibility.py`. Keep group leaves outside its
top-level `*.json` receipt scan. `stage-release-publication.py` checks exact
source/profile/platform coverage against frozen selection before staging and
signing. Update `release.yml` subject verification, upload and retention asset
lists for the index and referenced companions/leaves. Finalization must require
the validated signed index for this reserved version/source and all required
native profiles before `finalize-release.sh` can publish the draft. Old schema 1
readers still decode the actual legacy receipt; extra release assets confer no
new authority on them. Preserve `requireWorkerCoverage` in
`cmd/mesh-release-routes/workers.go` and its authenticated registry comparison,
including the existing 8 MiB registry bound. The companion validator strengthens
its digest-membership check to exact member coverage. Test the strict legacy
reader with the unchanged executable.

## Installed consumers and boundaries

`installed_releases.Releases.prove` uses the grouped wrapper and canonical
receipt/rollback plus companions, retaining its one-transition API. It must no
longer mistake companion JSON for another forward receipt. `installed_cohort`
accepts an explicit group and checks observed digests against that group.
The drivers in `installed_adoption.run_host_cases`, `installed_chain.run_case`
and `installed_publication.run_first_published` repeat the complete original
scenario in fresh states for each applicable group. Use group output directories
so `finish_case`, chain logs and cohort exports cannot overwrite one another.
Build/publish fixture releases once. Keep the custom-source addition in
`retain_custom_source`; validate it separately from immutable published members.

Each chain/waypoint group retains its original source workers through all hops,
helper replacements, first/final rollback and failed-edge restart checks.
`installed_waypoint.packet` uses that executing group's workers; the aggregate
still requires the complete registry union for every scenario. Keep original
reader rejection of the incompatible future manifest. The real first-published
updater path repeats success and rollback per group, preserving authenticated
intermediate observations. Per-case ledgers/counts remain actual local counts.
Terminal attachment, listener identity, route-gap, preservation and order checks
remain unchanged. Never widen readiness to accept a transient mismatch.

## Spotlight and path contracts

Create an owned `.noindex` parent before any build, extraction, artifact download
or executable copy on hosted Darwin. Use a short real root such as
`/tmp/mg-XXXXXX.noindex`; place native preparation/download destinations and
installed scratch beneath it. Keep packet contents and relative members
`tools/*`, `retained/workers.json`, `retained/release-*/mesh` unchanged. Update
both reusable workflows and artifact storage paths together, including cleanup
and `MESH_OWNED_WORKER_INPUT`. Validate resolved Unix socket lengths before
setup, retaining Darwin's 103-byte and Linux's 107-byte limits; do not append
group labels to socket paths. Cache keys, archive pins, source/preparation pins,
member checks and the installed artifact set contract stay intact.

Apple's current [Xcode documentation](https://developer.apple.com/documentation/xcode/locating-a-missing-debug-symbol-file.md)
explicitly identifies `.noindex` paths as a reason Spotlight cannot find files.
Use that suffix, not an unverified directory marker. Assert the owned resolved
parent has the suffix and exists before the first executable arrives. Retain
setup/path evidence and bounded before/after indexing-process observations in
the same next cycle. Setup is asserted; absence of every global `mdworker`, or
a speedup, is not. No `mdutil`, privacy-list edits or owner Mac configuration.

## Verification, cleanup and budget

Parent commissions a fresh independent Sol plan review before implementation,
then fixes routine findings. Total unit budget is 2 to 3 agent hours including
this short plan, implementation and review, with 1 to 2 hosted CI cycles.
Estimate 20 minutes for plan/review, 70 to 100 for implementation/regressions,
and 30 to 60 for validation, fixes and closeout. Record actual time. The closed
Fable round's two cycles buy no additional work here.

Add focused regressions for 31/32 members, byte-cap splitting, equal protocols
with different hashes, custom source, exact union, corrupted/swapped records,
missing profile/member/boundary, distinct group identities, interruption before
readiness and cleanup refusal. Wire them into packaging checks beside PR 459's
failure-evidence tests. Run required repository checks, including race tests,
gates and integration verification, once after implementation. Cycle 1 exercises
all selected sources on the existing three native platforms plus schema 1 and
installed oldest/chain/waypoint consumers on the same exact source. Cycle 2 is
for necessary fixes only. Keep five-second observer, eight hashing workers,
128 MiB image bound, six-second helper health, existing 30/90-second subprocess
bounds, chain 150 seconds, rollback 90 seconds, quarantine 45 seconds and
15-minute jobs. Repetition never resets an existing source-wide deadline.

Before setup inventory existing processes, services, scratch, mounts and caches;
list created resources and install teardown first. On success, error, timeout or
signal, export bounded partial evidence, stop owned workers/helpers/clients and
managers, verify absence on that host, then remove owned roots/downloads/tools.
Preserve active caches and pre-existing services. No next group after cleanup
failure. Small observations/log tails use seven-day CI retention, native packets
one day. Keep this plan and the compact result/reproducer in the report folder;
parent owns removal with lane closeout. List any retained resource, size and
removal condition. No paid runner, seed/native/CI run during planning, production
edit, PR, remote product push, release or fleet change in this design task.

Success requires exact coverage and all assertions passing within existing
bounds, verified signed aggregate gating, unchanged legacy-reader acceptance,
preserved Linux failure evidence and confirmed teardown. EPERM remains a
separate blocker if reproduced; grouping must neither hide it nor count a
partial run as proof. Stop after two cycles or the unit budget and report the
remaining blocker to the parent, without adding another diagnosis round.
