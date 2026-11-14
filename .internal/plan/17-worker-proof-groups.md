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
Published references are filename-only: `<sha>.worker-groups.json` for a
source companion and `<sha>.worker-group.json` for typed observation, receipt
reference and cleanup leaves. Keep local files beneath a separate
`groups/leaves/` directory, with an explicit basename mapping for upload.
No release reference contains a directory or traversal. Keep canonical legacy
receipts at their existing top-level names and exclude group documents from
the ordinary receipt scan. Documents stay bounded to 1 MiB and selection to
4096 members. Never flatten PIDs into a combined cohort.

`mesh-worker-groups.json` is an additional signed subject. Its header binds
the reserved version, release source, exact manifest digest, candidate images,
preparation and harness sources, registry digest, and complete expected native
source/platform/profile matrix. Its flat asset references bind bytes by digest
and size. Retain existing reader-packet and registry bounds; fail closed if
complete evidence exceeds a bound rather than omitting members.

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

Name one shared `validate_group_assets` boundary in
`fixtures/release_worker_groups.py`. It receives expected context, authoritative
selection and an asset resolver, verifies every required group/label/leaf and
returns verified coverage. Collect, stage, the real draft loader at
`installed_publication.py:51-60`, and finalization before
`finalize-release.sh:33` must call it. The post-sign path also verifies the
index's expected attestation subject through the same trust boundary as the
reserved metadata. A signature alone cannot authorize missing coverage or
wrong context.

Update `collect-update-proofs.py` before exposing canonical assets to
`assemble-release-compatibility.py`. Stage validates the complete native
transition matrix before signing the draft. Installed completion runs only
after signing, as the current release dependency requires. Its separate
completion evidence binds the verified signed index digest and exact required
installed source/platform/scenario/group matrix. Finalization requires both
pre-sign native coverage and successful post-sign installed completion. Do not
pretend installed results were already present in the signed index or mutate
signed assets after testing.

Extend `acquire-release-history.py`'s acquisition allowlist for
`mesh-worker-groups.json` and the two content-addressed suffixes. Download and
bound their actual bytes, not inventory-only names, and resolve the complete
required leaf closure with the same validator. Update upload, retention,
signature-subject verification and local-to-flat asset mapping together.
Regressions reject a missing index, validly signed wrong context, missing or
swapped leaf, incomplete matrix and a reacquired asset set without bytes.

Old schema 1 readers still decode actual legacy receipts; extra assets confer
no authority on them. Preserve `requireWorkerCoverage` and its authenticated
registry comparison, including the 8 MiB bound. Strengthen digest membership
to exact member coverage with the validated companion. Test the unchanged
strict legacy-reader executable.

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
remain unchanged. Update the top-level cleanup-record scan in
`prove-installed-catchup.py:88` for group output directories. Never widen
readiness to accept a transient mismatch. `firstPublishedUpdater` is currently
null: preserve its existing condition and report that real first-published
execution is unavailable, rather than inventing a published fixture baseline.

Publish the reviewed grouped harness as immutable source H, then update both
CI and release reusable workflow references and both `harness_source` inputs
from d831 to H, plus the matching wiring-test PIN, in one caller checkpoint.
H must already contain the grouped drivers, validators and path/export code.
Keep `worker_artifact_source` independently bound to the actual preparation
commit. Prove the executed harness is H and grouped before the first acceptance
cycle; editing only the local workflow is insufficient. Any reviewed harness
correction needs the same consistent five-reference refresh.

## Spotlight and path contracts

Create owned `.noindex` roots before any build, extraction, artifact download
or executable copy on hosted Darwin. Native packet roots can own their ordinary
tools subdirectories. Installed cases must use a short owned directory such as
`/tmp/mg-XXXXXX.noindex` directly as the installed root: do not set it as
`MESH_SHORT_TMP` and let the existing allocator add another `mi-XXXXXXXX`.
That nesting produces a 109-byte physical Darwin socket; the direct root gives
97 bytes. Change allocator ownership, validation and teardown together.
Group labels belong in evidence directories, not socket roots.

Keep relative packet members `tools/*`, `retained/workers.json` and
`retained/release-*/mesh` unchanged. Update both reusable workflows, artifact
paths, cleanup and `MESH_OWNED_WORKER_INPUT` together. Validate resolved physical
socket lengths before setup, keeping 103/107-byte Darwin/Linux limits. Preserve
cache keys, archive pins, source/preparation pins and member/artifact checks.

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
15-minute jobs. Repetition never resets an existing source-wide deadline. Track native source
600 seconds, each installed `Releases.prove` 600-second call, the existing
150/90/45-second phase limits, and the fixture signer's overall 600-second
lifetime across builds, proofs and cases. Do not restart that signer to extend
its clock. Record total phase times in cycle 1; if repetition cannot fit the
existing signer or 15-minute job, stop and report the limit instead of extending
or silently resetting it.

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
