# Mesh CI Slim Step 3 qualification

The release finalizer still requires the pinned installed signed-draft gate and all original reader jobs. The draft PR's replay uses hosted Linux AMD64, Linux ARM64 and Darwin ARM64 jobs with read-only Actions and contents permissions. It takes the immutable harness SHA from the PR head and the release version and source from an existing main run.

## Available inputs

Main run 44697623433, attempt 1, signed the v0.1.347 draft from `99b063babe2d2182d389a08a45668ed63efbd067`. The signed snapshot ZIP was downloaded and matched its GitHub SHA-256. Its reserved release ID is 452687504. The original reader jobs passed on all three platforms. The old installed gate failed, and GitHub skipped the publication finalizer. The public release endpoint returns 404 for v0.1.347. [Main run](https://github.com/ShaulLavo/mesh/actions/runs/44697623433)

The three `update-native-*` uploads from that run are gone after their one-day retention. Readers-only mode uses the surviving signed snapshot, signed trust, publication inputs, policy documents and exact native source tools. It cannot emit a qualifying installed completion. The replay passed on all three platforms at `9a6086db`. Its input is retained in [the readers example](mesh-ci-slim-publication.readers.json) so subsequent draft pushes do not repeat that run. Main run 44912521100 reserved v0.1.349 and had yet to produce a signed draft artifact at the time of inspection. [Reader replay](https://github.com/ShaulLavo/mesh/actions/runs/44944562006) · [Main run](https://github.com/ShaulLavo/mesh/actions/runs/44912521100)

## Run full qualification

1. Pick a completed trusted main push run with an unpublished signed snapshot and all three native preparation archives still available. Signing must have succeeded. The current writer attempt must have an explicit skipped publication finalizer, and no attempt can have started one.
2. On the combined draft PR branch, copy `.internal/evidence/mesh-ci-slim-publication.readers.json` to `.internal/evidence/mesh-ci-slim-publication.json`. Replace its run, release ID, version and source and set `mode` to `native`. Supply the seven archive names in the example and `update-native-linux-amd64`, `update-native-linux-arm64` and `update-native-darwin-arm64`. Use each archive's exact positive ID, compressed size, SHA-256 and producing attempt. A successful earlier job can supply an archive to a failed-jobs rerun.
3. Push the updated input file to the same draft PR. The replay authenticates the writer run and attempt, checks all pinned archive bytes, and derives the reader plan from the writer's policy inputs and signed draft. It runs the installed cases and all permitted original reader edges on their native platforms. The last job requires the exact three artifact IDs and attempts from their producing jobs, reauthenticates the signed draft and validates the installed, reader and cleanup evidence against the plan.
4. Record the replay result, queue times, job times and cache outcomes. Remove the optional input file from the draft after verification. The existing finalizer accepts only evidence from the publication path. A successful replay is evidence for a reviewed cutover.

To see one artifact and the jobs that can have produced it:

```sh
gh api "repos/ShaulLavo/mesh/actions/runs/$writer_run/artifacts?name=$artifact_name" \
  --jq '.artifacts[] | {id,size: .size_in_bytes,digest,created_at,expired}'
gh api "repos/ShaulLavo/mesh/actions/runs/$writer_run/attempts/$attempt/jobs?per_page=30" \
  --paginate --jq '.jobs[] | [.name,.conclusion,.started_at,.completed_at] | @tsv'
```

## Baseline timings

The v0.1.347 signing job took 10m 36s. Its original reader jobs ran for 33s on Linux AMD64, 43s on Linux ARM64 and 35s on Darwin ARM64. Reader admission after signing took 25s, 10s and 5m 49s respectively. The plan's older sample recorded an exact AMD64 native cache hit with about 27s spent restoring the archive. [Signing job](https://github.com/ShaulLavo/mesh/actions/runs/44697623433/job/136343742979), [baseline cache job](https://github.com/ShaulLavo/mesh/actions/runs/44686718341/job/136237787684)

## Completed native qualification

Full three-platform native qualification passed on 2026-11-22 in [run 44985812293](https://github.com/ShaulLavo/mesh/actions/runs/44985812293) at `943e9078`, replaying authentic unpublished signed draft `v0.1.350` (release ID `454041269`, source `f6212be330ab3a5de71ddfd024068ccc251c9efa`, writer run `44938130233`, attempt 1, explicit skipped finalizer) with all ten pinned artifacts:

- Total wall time: **13m 16s** (5 jobs total).
- `Check replay controls and authenticate writer inputs`: 39s (queue 38s).
- `Replay native (linux/arm64)`: 3m 33s (queue 5s).
- `Replay native (linux/amd64)`: 7m 58s (queue 8s).
- `Replay native (darwin/arm64)`: 9m 28s (queue 3s).
- `Verify exact three-platform qualification evidence`: 44s (queue 1m 43s).
