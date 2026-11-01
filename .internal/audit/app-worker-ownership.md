# App worker ownership reproduction and design

Status: Approved bounded implementation. Reproduction completed November 1, 2026. The managing decision below authorizes the corrected private repair after the verified 003 merge.

Design status: Approved for bounded implementation by the managing decision on November 1, 2026, after two independent Sol reviews and coordinator reconciliation of their specified findings. Neither historical review is represented as an APPROVE verdict. The Opus-authored architecture is retained with the coordinator corrections below. The production PR still requires failing-first evidence, an independent code review, all repository gates and merge.

Design source: `origin/main` at `c47b35ce8a6e6e309bec9b99064230873ed86e69`, with immutable proof commit `4c0121d9dbfed459724ce014494340e92798b8c3` imported unchanged as the design branch's parent. The design lane is `/home/shaul/.t3/worktrees/mesh/t3-ce0456b1`, branch `t3/audit-002-app-worker-design`. Design evidence is in `/work/reports/wave-20261101/mesh-audit/002-design/`.

## Source and scope

Execution base and audited source are `c47b35ce8a6e6e309bec9b99064230873ed86e69`. The supplied clean lane was fetched and based on current `origin/main` before edits. Both exact Plan 002 drift guards were empty before edits. The fixture, recovery-test, and module dependency comparison was also empty.

Worktree is `/home/shaul/.t3/worktrees/mesh/t3-8ce33594`. Branch is `t3/audit-002-worker-ownership-repro`. Only these repository paths change:

- `internal/apps/origin_worker_ownership_test.go`.
- `internal/daemon/apps_ownership_test.go`.
- `.internal/audit/app-worker-ownership.md`.

All production code, Plan 07, dependencies, versions, baselines, and live Mesh state remain unchanged. Plan 003 owns `internal/apps/origin.go`. This intentionally failing proof stays in a local commit. It must not be pushed or merged before the reviewed repair makes it green.

## What the runtime proves

`TestAppPostLaunchFailureStopsKnownWorker` calls the real `Origin.Handle` with a static create, uploaded fixture source, and a setup recipe. The path reaches `create`, `runSetup`, `start`, `failCreate`, and `cleanup`. No shell or server runs.

The worker fixture keeps liveness independent of label discovery. `Start` sees the extracted `index.html`, records synthetic worker `7K3D` as alive, and returns that ID with a sentinel publication error. `Find` succeeds with no match for both app labels. Only `Stop` addressed to the exact ID can remove that synthetic worker during the operation.

The actual operation returns the wrapped sentinel error. Rollback makes both successful empty label lookups and makes no direct stop. Worker `7K3D` remains alive while its previously observed workspace file is absent. The expected safety assertion fails. Fixture cleanup then clears its own synthetic worker and verifies that the origin has no operation gates left.

The result agrees with the source trace. `Origin.start` at `internal/apps/origin.go:504-511` returns an empty ID on error. `runSetup` at `:1148-1155` returns before recording a session. `failCreate` at `:1325-1328` calls cleanup. `stopLabel` at `:1486-1497` treats successful empty discovery as success, and cleanup at `:1499-1523` removes app files.

`TestAppWorkersPreservePostLaunchIdentity` calls the actual daemon adapter and lifecycle. Its test-local equivalent of `unpublishedLifecycle` uses an empty catalog with a sentinel reconciliation failure. Its launched worker is a synthetic Unix-socket fixture with running metadata, not a real process. The adapter returns exact ID `7K3D` and a `publicationError` wrapping the sentinel. `Find` succeeds empty while `sessionExit` still reports no exit. Direct `Stop` sends `protocol.TypeKill` through the production `connectOwned` path and leaves exit metadata.

The adapter fixture has a five-second accept deadline, a three-second connection deadline, and a five-second cleanup join. Closing the listener and joining the combined accept/handler goroutine is explicit. `Origin.Close` is not used as worker teardown.

## Commands and retained evidence

Evidence is under `/work/reports/wave-20261101/mesh-audit/002-reproduction/`. `run.sh` records exact commands, exit statuses, resource ownership, host census, and cleanup receipts. The PC heavy runner admitted the suite with no wait. No Boat upload or provider call was needed. Go was `go1.27.3 linux/amd64`.

All Go commands use the mise shim PATH, `GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local`, `GOPROXY=off`, and `GOSUMDB=off`. Existing Go caches are on `/work/cache`. Scratch, fixture files, build temporary files, and short sockets are under one owned `/work/tmp/m002-*` directory. Outer process deadlines supplement the unchanged package deadlines.

| Command | Outcome |
| --- | --- |
| `go test -race -count=1 -timeout=60s ./internal/apps -run '^TestAppPostLaunchFailureStopsKnownWorker$'` | Expected exit 1. `rollback deleted the workspace while known post-launch worker 7K3D remained alive; empty catalog discovery did not establish exit`. No unrelated setup or storage failure. |
| `go test -race -count=1 -timeout=60s -v ./internal/daemon -run '^TestAppWorkersPreservePostLaunchIdentity$'` | Exit 0. ID plus error, successful empty discovery while running, exact-ID kill, exit metadata, and joined fixture goroutine. |
| `go test -race -count=1 -timeout=180s ./internal/daemon -run '^Test(Labelled\|OwnedStop\|DemandStopsAnUnpublished)'` | Exit 0. Existing lifecycle contracts remain green. |
| `go test -race -count=1 -timeout=180s ./internal/apps -run '^Test(CandidateRollbackWaitsForStorage\|CleanupWaitsForStorageWhenRootStillExists\|RetiredUploadIsNotRevivedByItsWriter)$'` | Exit 0. Existing app storage and operation guards remain green. |
| `git diff --check` and exact three-path scope check | Pass. No production dependency changes. |

The pipe characters in the table are Markdown-escaped. The `.command` files contain the executable arguments without table escaping. No broad suite, integration campaign, release, or baseline update ran. The normal commit hook invokes scoped `scripts/gates.sh --fast`. Its first attempt found ten new test-only lint findings. The fixture now uses a confined source root and wraps its socket errors. No baseline changed.

## Observed transitions

This table records current behavior. It does not select a repair structure.

| Point | Identity and evidence | Current result |
| --- | --- | --- |
| Uploaded source becomes setup workspace | Extracted source is present when fixture `Start` runs. | Setup is reached through the real create path. |
| Worker start returns | `7K3D` is alive in the fixture and accompanies the sentinel error. | `Origin.start` returns an empty ID with the wrapped error. |
| Setup unwinds | Error returns before successful-session bookkeeping. | `failCreate` starts rollback. |
| Cleanup discovers labels | Both `Find` calls succeed with no match. | No `Stop` call occurs. |
| Cleanup removes workspace | Worker liveness is unchanged. | Previously present source file is gone. |
| Adapter receives direct stop | Catalog remains empty, but exact ID addresses the Unix socket. | Kill is received and exited metadata is recorded. |
| Test teardown | Synthetic fixture state, listener, connection, goroutine, and scratch are owned by this run. | All are released. |

## Crash-point limits

These are source-derived limits for the design handoff, not crash tests or an approved durability design.

| Crash point | What this proof establishes | Still unproved |
| --- | --- | --- |
| Before worker start | No worker was launched by this fixture yet. | General crash recovery is outside this reproduction. |
| After worker start returns ID plus error | The caller has a possibly live exact ID. The app helper discards it. | How ownership survives a crash or failed save. |
| After the helper returns the error | Successful-session bookkeeping has not run. | Restart recovery of this failed attempt. |
| After empty discovery, before deletion | Empty catalog discovery coexists with a running fixture worker. | A safe persistence and stop order under every interruption. |
| After workspace deletion | This run still observes the synthetic worker alive. | Historical process state, frequency, and deployed impact. |
| After direct stop | This fixture receives kill and records exit. | Crash behavior between stop, ownership-clear save, and file removal. |

## Plan 07 disposition

The existing `.internal/plan/07-quality-and-security-wave.md:256-263` observation stays open and unconfirmed. It concerns a real setup worker left after interrupted-update recovery. The original logs and worker metadata were removed. The requested PID, `/proc/<pid>/stat`, candidate counts, and origin/worker logs cannot be recovered from this test.

The new proof concerns failed static create after ID-plus-publication-error, successful empty catalog discovery, and synthetic liveness. It reaches the same setup-stop boundary but does not reproduce interrupted update, restart, or a real running, blocked, or zombie process. It neither establishes nor rules out a common cause. Attach this proof to the existing investigation during design. Do not close or rewrite Plan 07 from this result. No duplicate investigation or issue was opened.

### Design-stage disposition

The Plan 07 item stays open, and its historical cause stays unconfirmed. This design adds one source fact and one conditional consequence.

The source fact is that `runSetup` saves `Candidate.Session` at `origin.go:1152-1157` only after `Origin.start` returns successfully, and no recovery path reads it. `resolveCandidate` at `:1371-1402` stops by label only. The comment at `:1141-1142` says the session is recorded "so a crash leaves it findable", and nothing consumes it.

The integration fixture does not show whether that save happened before its kill. `integration/temporary_app_update.sh:147-155` kills the daemon as soon as a `meta.json` names a live setup PID. The worker writes `meta.json` before it opens its socket and removes its launching marker (`internal/worker/worker.go:517-543`). So at the kill, the launcher may not have returned, and the origin may not have saved either `Candidate.Session` or any new ownership record. A worker still behind its launching marker is also skipped by the startup catalog scan (`internal/daemon/catalog.go:182-184`).

The conditional consequence is this. When the repair below has durably recorded a setup worker, candidate rollback stops it by its exact recorded identity before any label lookup. That removes one possible cause of the Plan 07 symptom, catalog discovery missing the setup worker at recovery time, but only for attempts whose record was saved. An attempt killed before that save still depends on label discovery, as today. Other possibilities stay untouched and unranked. Examples are a first maintenance pass that has not finished inside the script's 10-second poll, the stop budget, and the running, blocked, and zombie states the item asks to tell apart. The implementation PR links this note from the Plan 07 item and leaves the item open. A passing `integration/temporary_app_update.sh` run is one more data point, not a cause. No new broad recovery investigation is assigned.

## Design

This section is revision 2 with coordinator corrections from the independent review at `/work/reports/wave-20261101/mesh-audit/002-review-rev2/REVIEW.md`. That review requested changes at `137d3ba506fdc91b5359210f7fe3a3e27744e0e1`. Findings 1 to 3 specify corrections inside the existing marker architecture; finding 4 corrects the owner-facing limits. The original design source remains `c47b35ce8a6e6e309bec9b99064230873ed86e69`. Technical reconciliation did not itself authorize production. The subsequent managing decision below accepts the policy limits and authorizes this bounded repair, preserving independent code review and merge gates.

Revision 1, committed at `c57f2439dab43094693928ebed77458059ff4a8e`, received CHANGES REQUESTED from the independent Sol review at `/work/reports/wave-20261101/mesh-audit/002-review/REVIEW.md`. Its findings 1 to 3 showed that an ID-only stop contract, unchanged lifecycle retention, and per-app operation ownership could not deliver the promised guarantees. Revision 2 changes the architecture in three places and widens the private scope to the worker launcher, the catalog, and the lifecycle. The coordinator should treat the changed scope as needing approval with the fresh review.

### What changed from revision 1

| Review finding | Revision 1 | Revision 2 |
| --- | --- | --- |
| 1. A reusable four-character ID is not an identity | Prefix check on metadata label, fail-open on unreadable metadata, check-then-connect gap | An owner marker written into the session directory at reservation, before the worker starts. It holds the full app label and a random token. The origin records that token and compares the full marker before any exact-ID stop or wait. A missing or unreadable marker never leads to a stop. |
| 2. Retirement can delete exit evidence before the clear is durable | Moved `Forget` only | The marker also pins the directory. Catalog retirement and explicit removal refuse marked directories, so the directory, its ID, and its exit evidence stay until the origin durably records the end and then releases the marker. |
| 3. An unowned safety stop can outlive revocation and stop a newer launch | Assumed per-app operation ownership | A per-app fence registered when `preempt` times out. No new app operation begins until every fenced safety stop has returned. Entries also carry a generation, so a stale remover cannot erase a newer entry. `start` rechecks revocation after its slow gate. |
| 4. Regression gaps | 17 rows, placeholder selector | Named tests with exact assertions, and a selector command that fails when a name is missing. |

### Decisions

1. **Owner marker.** For app workers only, reservation creates a private immutable `.owner` containing the full label and a 128-bit random token from the adapter. Before `cmd.Start`, write the complete marker, sync its file, check close errors, sync the session directory, then sync the sessions parent that reserved the directory. Any write, close or sync failure aborts before spawn and cleans only this reservation. Report cleanup failures and retain their evidence; never claim a child started or silently treat an uncertain reservation as absent. A normal pre-start failure removes and durably retires its own reservation.
2. **Retention.** `retireFinishedSessions`, `Catalog.Remove`, and `removeRetiredSession` skip or refuse a directory holding `.owner`, or one whose marker inspection is malformed or unreadable. Only definite marker absence permits unpinned removal. Check before retiring the database row, before the `.forgotten` fast path, and again at physical deletion. An error at an earlier check must not fall through to a later deletion. Because session reservation uses `Mkdir` (`internal/worker/launch.go:200-214`), a marked directory's ID cannot be reused, and its metadata, exit record, and socket path keep naming the same incarnation until the marker is released.
3. **Ownership record.** The origin records `{ID, Label, Token, Gen, Ended}` per app in `originState.Launched`, beside `Apps`. `Gen` comes from a persisted per-origin counter. `Token` comes from `Workers.Owner(ID)` right after `Workers.Start` returns. A failed `Owner` call still records the entry, with an empty token, so the ID stays owned.
4. **Identity before action.** Every exact-ID `Stop` or `Wait` first reads `Workers.Owner(ID)`. A tokenless entry remains unresolved. When the first usable marker names its full label, bind the complete token under `mu` to that same `{ID, Gen}`; never overwrite a different nonempty token. Snapshot that bound marker for the action and keep its reservation through the entire action and error unwinding. Persist the complete marker in the ended record before any Release. No label-only or freshly reread-marker release is allowed. The marker cannot change between that read and the stop, because only the origin releases it and the origin never releases a marker while any stopper of that entry is outstanding (decision 7).
5. **Two-step end.** A verified end sets `Ended` on the fully bound entry matching `{ID, Gen}`. The operation-held `drainEnded` below always acknowledges an ended-state save before expected-marker Release, then acknowledges removal before Forget. An in-memory Ended flag is not evidence of a committed save. An ended entry is never stopped again. A durably running entry retains its reservation and exit evidence until an acknowledged ended save.
6. **Launch gate.** `start` refuses a launch while any running entry for that label cannot be verified ended. It rechecks revocation after that gate and again after the launch, as today.
7. **Release rule.** Only the app-operation holder releases a marker, while no fence or outstanding Stop/Wait can act on that entry. Admission to release and registration/join of safety work must enforce this exclusion under `mu` through actual I/O completion, not merely through timeout expiry. A fence-blocked drain retains entries and returns a retryable error; the next holder must invoke the drain.
8. **Fence.** When `preempt` gives up on a revoked operation that has not yielded, it registers a fence for the app under `mu` while that operation still owns the app. `beginOp` waits for the fence. `tryOp` reports the app busy. A later `preempt` waits on the fence like an operation, and joins it after `yieldBudget`. The fence is released when that safety stop returns. The revoked operation itself is not blocked, but it cannot release markers while the fence exists.
9. **Label fallback.** After exact IDs, run Find(label), excluding every handled ID, including proven mismatches. For another found ID, read and validate its full expected app marker before action, create a generation-scoped claim for that exact marker, then stop under the same reservation and completion-drain contract. Marker errors retain an unresolved claim and fail closed. A mismatch never permits Stop or Release. The app-operation holder durably records verified Ended with the saved marker before releasing it. A fenced safety caller can mark end but cannot release it. Do not read a current marker only after Stop and use it as new release authority. This handles catalogued attempts whose original launch save was lost, not invisible never-ready attempts.
10. **Recovery adoption.** `recover` returns early when the catalog lists a live server (`origin.go:1857-1862`), and `ensureServer` adopts a found server (`:1220-1233`). Neither calls the gate. A failed launch that the catalog later lists as live can therefore be adopted as the server, and its entry becomes the server's ownership record. The design promises that every failed launch is stopped before a replacement launch under its label, during rollback, and during cleanup. It does not promise to stop a failed launch that later serves.

### Data shape

```go
// launchedWorker is a worker an app operation started. It stays until the
// worker is proved ended and its owner marker is released, so rollback can
// stop it by identity even when no catalog lists it.
type launchedWorker struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Token string `json:"token,omitempty"`
	Gen   uint64 `json:"gen"`
	Ended bool   `json:"ended,omitempty"`
}

type originState struct {
	// ... existing fields unchanged ...
	Launched  map[string][]launchedWorker `json:"launched,omitempty"`
	LaunchSeq uint64                      `json:"launchSeq,omitempty"`
}
```

Whole-record writes to `Apps` at `origin.go:898,1105,1173,1351` never touch `Launched`. Entries change only by append under `mu` and by update or removal matched on `{ID, Gen}` under `mu`.

Two bounds hold separately. First, at most one running entry per label exists after any successful `start`, because the gate refuses to launch while an earlier running entry is unresolved. Second, every launch that returned an ID keeps an entry until it is proved ended, because removal matches `{ID, Gen}` and only follows a verified end. Ended entries are retried by the explicit operation-held drain call sites below, including healthy recovery and before replacement launches. Repeated successful restarts must not accumulate ended entries or pinned directories. No small constant bounds the number or bytes of crash-orphaned reservations. The list is cleanup intent for real app launches. It is not a session catalog.

### Private interface changes

```go
// internal/apps
var ErrNoOwner = errors.New("app: worker has no owner marker")

type Workers interface {
	// existing Start, Stop, Find, Forget, Processes unchanged
	// Owner reads the marker written when the worker was reserved. It wraps
	// ErrNoOwner when the session directory or its marker is absent.
	Owner(ctx context.Context, id string) (string, error)
	// Release compares the complete saved expected marker and durably removes
	// only that marker. Missing or different identity is an idempotent no-op.
	// Read, unlink and directory-sync failures remain errors for retry.
	Release(ctx context.Context, id, marker string) error
}
```

`Start` and `Stop` keep their signatures. The unchanged proof test overrides exactly those two methods, and the origin must keep calling them for the proof to observe its behavior. The marker and the origin's identity check carry the expected full label and incarnation instead of a changed `Stop` signature.

The daemon adapter's `Start` generates the token and passes the marker content through `startLabelled` and `creationRequest` into `worker.LaunchConfig.Owner`. Demand routes pass no marker. `Owner` reads and validates `paths.Owner(dir)` through an `os.Root` on the sessions directory. It distinguishes definite absence from malformed or unreadable content. `Release` uses only the entry's saved complete expected marker. A match permits unlink, followed by session-directory sync before success. If unlink succeeds but sync fails, return an uncertain-release error and retain the durable ended entry. On retry, missing marker still requires syncing the surviving directory before success; if the directory is gone, sync the sessions parent. A different marker is preserved and never substituted as release authority. The reservation durability contract makes a replacement a distinct incarnation. `Stop` stays unchanged.

### Identity and stop outcomes

| Marker read by `Owner(X)` | Stop result | Entry | Files and record |
| --- | --- | --- | --- |
| Full label and token match | `Stop(X)` returns nil | `Ended`, then released and removed | Cleanup may continue |
| Full label and token match | `Stop(X)` returns an error, for example missing metadata and no socket | Running | Kept. Error names X. |
| Full label matches, recorded token empty | Bind the complete observed token to the same `{ID, Gen}` before action | Persist that exact token with Ended before Release; on failure keep the claim | No label-only release, including after reopen |
| Marker names another label or token | No stop. The directory is a proven different incarnation. | Removed without a stop or release | Cleanup may continue |
| `ErrNoOwner`, the directory or marker is missing | No stop | Running | Kept. Error says X's identity cannot be proved. |
| Any other read error | No stop | Running | Kept |

A different marker proves a different incarnation because the origin releases a marker only after the matching entry is durably `Ended`, and the catalog cannot remove or reuse a marked directory before that. A missing marker on a running entry proves nothing. Malformed, unreadable and missing marker outcomes all stay fail-closed. Correct durable reservation prevents normal marker loss after an acknowledged entry save; fault tests must establish that order instead of assuming metadata persistence makes the marker durable. Missing metadata, a dead socket, an empty `Find`, and a timeout are never treated as an end.

`Wait(X)` keeps today's meaning, an exit status for that ID. It needs the same identity read first. `appWorkers.Wait` reports an error for the ended-without-status case at `internal/daemon/apps.go:143-147`, so only a returned status clears setup ownership. `Stop` also accepts the process-group-gone case at `demand_sessions.go:124-134`. Both are evidence about the marked incarnation, which is why the marker must outlive the clear.

### Order for one entry and completion drain

1. `Workers.Start` returns X. Read Owner, append the identity claim and generation, and save under the bounded cancellation-independent maintenance context. Owner failure retains a tokenless unresolved claim and returns an error. It grants no release authority.
2. A stopper snapshots running entries under `mu`, binds any first usable complete token to the same generation, checks identity and calls Stop outside `mu`. Only verified end sets Ended on that exact claim. Keep the complete expected marker with it. A proven different incarnation may discharge the old claim without acting on or releasing the current occupant. Include every handled ID in fallback deduplication.
3. One private operation-held helper, `drainEnded(ctx, id)`, owns all ended-entry completion. It excludes fenced or outstanding worker actions, snapshots complete ended claims and saves their current ended state successfully before any Release. It repeats this save even if a previous failed save or safety stop already set Ended in memory. An unrelated whole-state save, including upload expiry, is not its acknowledgement barrier.
4. The helper calls Release with the saved exact marker outside `mu`. A failure before unlink or after unlink retains the ended claim. A retry never Stops an ended claim or rereads a new marker as release authority.
5. After successful Release, remove only the matching `{ID, Gen}` under `mu` and save. On a failed write or lost acknowledgement, restore only that same ended claim before unlocking, not an old app or whole-state snapshot. It is safe for a later whole-state save to retain this ended tombstone. Retry expected Release and removal; a missing or different marker cannot kill or unpin a replacement.
6. Forget runs only after acknowledged removal and the enclosing operation's final record save. Workspace deletion still requires all affected workers proved ended or a proven different incarnation. Unknown identity or stop outcome keeps the workspace.

The helper runs only from lexical app-operation holders. `beginOp` and `tryOp` keep ownership-admission duties and do not perform worker I/O inside `mu` or hide drain errors in a boolean result. Assign these call sites explicitly:

| Call site | Required drain point and error result |
| --- | --- |
| `start` | After resolving earlier running claims, before the revocation recheck and before Start of a replacement. A drain error blocks that launch. No ended X is left beside a newly launched Y on success. |
| `runSetup` | After matching Wait returns a status and setup is marked ended, before successful setup returns or Forget. Failed setup also drains through its rollback. |
| Owned `stop`, `stopLapsed` and successful-preempt `overrule` | After exact stops and label fallback, before returning. Join stop and drain errors while making safe independent progress. |
| `resolveCandidate` | After stopping the candidate's selected labels, before deleting candidate files or clearing Candidate. Drain only ended entries; preserve the pre-swap live server and its running claim. |
| `cleanup` | Stop both labels, drain all ended entries, require no unresolved claims, remove app files, delete the app record and acknowledge that save, then Forget both labels. Return before deletion on unknown stop/drain outcomes. |
| `recover` | Immediately after acquiring `tryOp`, before any phase dispatch or healthy-server early return, and after later stops that produce ended entries. Keep the existing merged upload-expiry step unchanged. |
| `ensureServer` | Before its found-live/adoption early return and before replacement. All callers already own the app operation. Never release the adopted live server's marker. |
| `checkServing` | Immediately after acquiring its listener-check app operation, before any healthy return. |
| Fenced unowned safety `stop` | Mark only verified ends; no Release or drain. Keep the fence until all joined Stop/Wait/fallback actions return. The next actual holder reaches one of the assigned drain paths. |

A cancelled or fence-blocked drain retains claims and reports the error. It cannot silently return a successful completion with pending ended work. A same-process retry after save failure uses the latest generation-matched state. All original app whole-record writes remain confined to Apps.

### Fence schedule

The review's schedule runs like this under revision 2. Update A is revoked by an offline lease lapse and does not yield within `yieldBudget`. `preempt` registers the fence while A still owns the app, then `overrule` snapshots X and starts stopping it. A unwinds and releases its operation. The registry recovers. Update B calls `beginOp` and waits on the fence. The safety stop finishes X and its label fallback finds nothing new, because no new operation could launch. The fence releases. B then obtains its fresh signed inspect, stops X, which is already ended, writes its refreshed record, and starts Y. Nothing that ran before B's operation began can stop Y or erase its entry.

The fence uses the existing per-app `maintenanceBudget`, cooperatively. It stays registered until every joined safety action actually returns. A timeout is not permission to detach an uncancellable action or admit a new operation. Cancelled waiters cannot release another caller's fence. Registration that finds the revoked operation already yielded must acquire ownership or retry, not enter unfenced safety work. A blocked `Workers.Start` in A never holds `mu`, so the safety snapshot is not delayed by it. A worker A launches after the snapshot is stopped by A's own post-launch revocation check, as today.

### Transition table

`E` is the app's entries for one label. "Running" and "ended" refer to the entry flag.

| Event | `E` before | Action | `E` after | Files and record |
| --- | --- | --- | --- | --- |
| `start`, revoked or lease lapsed | any | Refuse before launching | unchanged | unchanged |
| `start` gate, running X | {X running} | Bind identity, Stop, drain Ended through both acknowledged saves, then recheck revocation | {} on success, otherwise retained | No replacement before completion |
| `Workers.Start` returns `("", err)` | none running | Nothing to record. Rollback by label as today. | unchanged | rollback |
| `Workers.Start` returns `(X, err)` | none running | Read owner, record, save. Return the error. | {X running} | rollback stops X before deleting files |
| `Workers.Start` returns `(X, nil)`, save fails | none running | Keep X in memory, fail the start | {X running} in memory | rollback stops X |
| `Workers.Start` returns `(X, nil)`, revoked meanwhile | none running | Record, then stop X | {X ended} or {X running} | rollback |
| `Workers.Start` returns `(X, nil)` | none running | Record and save | {X running} | continue |
| Setup exits 0 | {X running} | Identity, `Ended`, save, release, remove, save, `Forget` | {} | continue |
| Setup fails or times out | {X running} | `recordSetupFailure`, rollback | until rollback | rollback |
| Owned `stop`, `stopLapsed` | {X running} | Exact stop, fallback and operation-held drain | {} on success, otherwise retained | Unknown outcomes keep files |
| Fenced safety stop | {X running} | Same as owned stop. No save, no release. | {X ended} or {X running} | no record change |
| `resolveCandidate` | {X} | Save restored record, stop selected labels, drain, remove candidate files, clear Candidate and save | {} or retained | Candidate kept until stop and drain succeed |
| `failUpdate` restore | {X} | Whole-record write to `Apps` only | {X} | previous record restored |
| Delete or failed-create cleanup | {X} | Stop, drain, require no unresolved claims, remove files, delete record and save, Forget | {} on success | No file deletion on unknown stop/drain outcome |
| Healthy recover, ensureServer or listener check | {X running} plus older ended claims | Drain ended claims before healthy/adoption return; preserve X | {X running} | No accumulation of ended reservations |
| `ensureServer`, no server found | {X running} | Gate stops and fully drains X, then launches Y | {Y running} | No retired-X accumulation |
| Restart | durable `E` | Loaded. The Sync path that owns the app consumes it. | same | see crash table |

### Crash points

Workers survive a daemon crash (ADR 001). The origin runs inside the daemon, and startup reconciles the catalog before maintenance (`internal/daemon/app.go:413`), skipping launching reservations.

| Crash point | Durable state | After restart | Outcome |
| --- | --- | --- | --- |
| Before Start returns or before ownership save | No entry; each launched attempt has a durable marker | Preparing create requests deletion in pass 1 and cleans up on a later authoritative deleted pass; update recovery uses label discovery | Catalogued live attempts may be stopped. Invisible launching attempts may remain live and their workspace may be removed. Orphan pins have no small byte bound. See limitation 1. |
| After the entry save, before any stop | {X running} | Create: delete request, then cleanup on a later pass stops X by identity. Update: `resolveCandidate` stops X. Activating server: `recover` calls `activate`, then `ensureServer`, which adopts X if catalogued and alive or stops it through the gate and relaunches. | Stopped or adopted as the server. |
| After `Stop(X)` returned nil, before `Ended` was saved, including a failed write | {X running}, marked directory with exit evidence | The next stop checks identity and `Stop(X)` returns nil from the retained evidence. | Proved again. The marker kept the directory. |
| After the `Ended` save committed but its acknowledgement was lost | {X ended} | The next holder releases and removes. | No stop. |
| After `Release`, before the removal save | {X ended}, marker gone, directory may be retired and its ID reused | The next holder calls `Release`, sees a missing or different marker, and removes the entry. | Never touches the reused ID. |
| Release fails before unlink | Durable complete Ended claim; marker present | Assigned drain retries expected Release | No second Stop; evidence stays pinned |
| Release unlinks but directory sync fails | Durable complete Ended claim; marker presence uncertain after crash | Drain retries expected Release and required directory durability, then removal save | Never release a newly read replacement marker |
| Removal save fails before write or loses acknowledgement | Durable Ended or absent claim; memory retains the same ended generation until acknowledged retry | Same-process drain or reload repeats expected Release and removal | Missing/different marker is idempotent; no Stop of reused ID |
| After acknowledged removal, before file or app-record deletion | No claim; completion was verified and durably released | Cleanup reruns file and record work | No retained worker evidence is required for that completed claim |
| After the record deletion save, before `Forget` | Nothing | Ended unmarked session directories wait for a later `Forget` or catalog retention. | Harmless. |
| After a failed `Stop(X)` | {X running} | Every pass retries through the path that owns the app. No launch under that label. | Bounded. A Sync error names X. |

### Limitations

1. A crash before the ownership save can leave one orphaned marker per affected launch attempt, across several apps. Session directories can retain logs, scrollback and recovery data; this design gives no small byte bound. A catalogued live attempt can be discovered and handled, but an invisible `.launching` attempt may remain live or lose its workspace through successful empty label lookup. That pre-save limitation remains explicit. Never-ready unrecorded attempts lack exit proof. Manual recovery must establish the exact incarnation ended and all stop/release work quiesced before unpinning; deleting `.owner` blindly is not a safe procedure. Stronger pre-launch durable ownership or automatic recovery is outside the accepted bounded scope.
2. A recorded worker that died before writing metadata or opening its socket can never be proved ended. `Stop(X)` fails on every pass. The app keeps its files and record, and Sync reports the error each pass. This applies only to never-ready workers. A worker whose stop was already verified keeps its evidence under decision 2, so it is not in this class.
3. A host reboot leaves the marked directory. `sessionExit`'s process-group check can prove the end, unless the PID was reused by an unrelated group after the reboot. Then the stop fails closed as in limitation 2.
4. Restoring an older origin state over a newer one is unsupported. A restored running entry whose marker was released stays fail-closed under the "missing marker" row. It is never stopped and never cleared automatically.
5. The Plan 07 cause stays unknown, as stated above.

### Implementation scope

| Path | Change and why | Size estimate |
| --- | --- | --- |
| `internal/apps/origin.go` | `launchedWorker`, `Launched`, `LaunchSeq`, `ErrNoOwner`, `Owner` and `Release` on `Workers`, identity-checked `stopLaunched`, explicit `drainEnded` and its healthy/stop/replacement call sites, `start` gate and rechecks, `stopLabel` with app ID and fallback release, `runSetup`, `resolveCandidate`, `cleanup` order, the fence in `preempt`, `beginOp`, `tryOp`, `overrule`, and `yieldBudget` as an origin field so tests can reach the non-yield path | About 180 added, 30 removed |
| `internal/daemon/apps.go` | Token generation and marker in `Start`, `Owner`, `Release` | About 45 |
| `internal/daemon/demand_sessions.go` | `startLabelled` takes the marker. Demand passes empty. | About 4 |
| `internal/daemon/lifecycle.go` | `creationRequest.owner` into `LaunchConfig.Owner` | About 3 |
| `internal/daemon/catalog.go` | Skip marked directories in retirement selection and refuse them in `Catalog.Remove` and `removeRetiredSession`. Needed because decision 2 is the only way to keep evidence and ID until durable release. | About 15 |
| `internal/worker/launch.go` | LaunchConfig.Owner and durable reservation-time marker publication, including file/session-directory/parent sync and pre-spawn fault cleanup. | Estimate follows the durability contract; the earlier 12-line estimate is retired |
| `internal/paths/paths.go` | `Owner(sessionDir)` | 1 |
| `internal/apps/core_test.go` | `fakeWorkers` gains `Owner` and `Release` that know only IDs it launched | About 25 |
| `internal/daemon/apps_test.go` | `guardedAppWorkers` gains the two methods | About 4 |
| New `internal/apps/origin_launch_ownership_test.go`, `internal/daemon/apps_owner_test.go`, `internal/worker/launch_owner_test.go` | Regression matrix below | New files |
| `.internal/audit/app-worker-ownership.md`, `.internal/plan/07-quality-and-security-wave.md` | Review record and results. One link from the Plan 07 item, which stays open. | Notes |

The two proof tests stay byte-identical. No change to `internal/protocol`, the CLI command set, worker discovery, `go.mod`, `.gates/baseline.json`, or versions. A state file without the new fields loads empty values. No compatibility branch is added. Online physical removal of a pinned ended app worker must be deferred or refused. Existing offline forgotten-intent success remains deferred physical deletion by the managing decision. No CLI command or offline-result change is authorized.

With the unchanged `fakeWorkers.Owner`, the first proof's synthetic ID `7K3D` has no marker. The origin therefore refuses to stop it, keeps the workspace, and returns the failure. The proof accepts that fail-closed outcome. Its companion test below proves the stop-before-delete path with a fake that does hold a marker.

### Regression matrix

All app tests are in `internal/apps/origin_launch_ownership_test.go` unless named otherwise. Each registers cleanup before calling `Handle`, closes any reopened origin, and bounds every wait at five seconds. Assertions inspect worker identity, files, and the saved state in the store, not only error strings.

| Test | Required assertions |
| --- | --- |
| `TestAppPostLaunchFailureStopsKnownWorker` (unchanged proof) | Passes through fail-closed retention, as explained above. |
| `TestAppWorkersPreservePostLaunchIdentity` (unchanged proof) | Passes. |
| `TestLaunchOwnershipStopsBeforeDelete` | Failed static create with setup ID plus error and a marked fake. Inside `Stop`, the workspace file still exists. Afterwards the workspace is gone, the saved state has no app and no `Launched` entry, and the marker was released before `Forget`. |
| `TestLaunchOwnershipRecordedBeforeOriginStartReturns` | The saved entry with the fake's token exists before `Origin.start` returns, for both labels. The setup entry is removed after exit 0, and only then is `Forget` called. The server entry stays while serving. |
| `TestLaunchOwnershipEmptyIDFailure` | `Start` returns `("", err)`. No entry, no `Owner` call, rollback by label. |
| `TestLaunchOwnershipServerCreateFailure` | Server create, `app` label returns ID and error, empty `Find`. Stop observes the workspace present, then it is removed. |
| `TestLaunchOwnershipFailedUpdateBeforeSwap` | Setup ID plus error. Setup stopped by identity. Serving worker not stopped. Candidate removed. Previous record and `Session` restored. |
| `TestLaunchOwnershipFailedUpdateAfterSwap` | Replacement ID plus error is stopped before the previous server's single relaunch. |
| `TestLaunchOwnershipStopOutcomes` | Subtests `confirmed-exit` (cleared), `missing-metadata-and-socket` (Stop error, entry and files kept), `empty-find` (entry kept, never cleared by Find), `find-error` (stop fails, entry kept). |
| `TestLaunchOwnershipIdentity` | Subtests `other-app-label` and `other-token` (no `Stop`, entry removed, files may go), `ordinary-session` (no `Stop`), `missing-marker` and `unreadable-marker` (no `Stop`, entry and files kept). |
| `TestLaunchOwnershipStopFailureRetainsAndRetries` | `Stop(X)` fails until released. Files, record, `Candidate`, and entry kept. An update retry is refused, and no `Start` happens under that label. After release, one Sync clears the entry and removes files. |
| `TestLaunchOwnershipRecordSaveFailure` | The save adding the entry fails. `start` fails, and rollback stops X in the same process. |
| `TestLaunchOwnershipEndedSaveFaults` | Subtests `failed-write` and `lost-ack`, using the `appPhaseFaultStore` pattern from `origin_recovery_test.go:156-166`. Inspect the store, reopen the origin, run Sync. Failed write: durable running, X proved again from retained evidence, then released. Lost ack: durable ended, released without a stop. `Forget` never runs before the removal save. |
| `TestLaunchOwnershipRestartRecovery` | Subtests `preparing-create` (pass 1 requests deletion and keeps files, pass 2 stops X by identity and cleans up), `ready-update-candidate` (pass 1 stops setup X, previous server untouched), `activating-server` (adopted when found live, otherwise stopped by the gate and relaunched once). |
| `TestLaunchOwnershipSetupInterruptedRecovery` | Successful setup start, interrupted `Wait`, empty discovery after reopen. Recovery stops the setup by its recorded identity and removes the candidate. |
| `TestLaunchOwnershipCancelledCaller` | Request context cancelled while `Start` runs. The entry is saved anyway, and rollback stops X. |
| `TestLaunchOwnershipFencedSafetyStop` | `yieldBudget` set to 20 milliseconds. Update A blocks in an uncancellable fake `Wait`. An offline lapse makes Sync fence the app. The safety `Stop(X)` blocks on a gate. A is released and unwinds. The registry recovers. Update B with a refreshed lease starts and blocks in `beginOp`. The test asserts no `Start` yet, releases the safety gate, then asserts the event log order: safety stop returns, fence releases, B starts Y. Y is alive at the end, never stopped, and its entry remains. |
| `TestLaunchOwnershipStaleRemovalKeepsNewerEntry` | Direct generation-matched removal of `{X, g1}` leaves `{X, g2}`. Separately assert the fence prevents a newer production launch until the old stopper finishes; do not fabricate that forbidden schedule as permitted behavior. |
| `TestLaunchOwnershipRecordedAndDiscoveredStopOnce` | The entry X is also what `Find` returns. Exactly one `Stop(X)`. |
| `TestLaunchOwnershipWholeRecordRestoreKeepsEntries` | `failUpdate`'s restore keeps the entry. |
| `TestLaunchOwnershipOneRunningEntryPerLabel` | Repeated failing launches with a failing stop leave one running entry per label and no second `Start`. |
| `TestLaunchOwnershipFoundUnrecordedReleasesMarker` | Read and bind the found worker marker before action. Stop observes that reservation; acknowledge a complete Ended save before expected Release, then acknowledge removal. A fenced path defers Release to the next holder. Failures retain the matching claim, and other-label/token occupants are never stopped or unpinned. |
| `internal/daemon/apps_owner_test.go: TestAppWorkersOwnerMarker` | Subtests `written-at-reservation`, `release-matching-only`, `release-missing`, `owner-missing-directory`. |
| `internal/daemon/apps_owner_test.go: TestCatalogKeepsOwnedSessions` | Age retirement, count retirement, a forgotten marker, and `Catalog.Remove` all leave a marked ended directory. Each removes it after `Release`. |
| `internal/worker/launch_owner_test.go: TestLaunchWritesOwnerBeforeStart` | The marker exists when the child starts. A failed `cmd.Start` removes it with the reservation. |

### Required review-correction assertions

These extend the named groups above without creating a placeholder selector. They are requirements, not completed test results.

- Identity: initial Owner error, later full-token binding, ended save and reopen, same-label different-token replay. Assert no Stop and no marker Release of a replacement, not just final worker liveness.
- Marker durability: fail marker write, close, file sync, session-directory sync and sessions-parent sync before spawn. Assert no child starts and only the owned reservation is cleaned or explicitly reported retained. Malformed and unreadable marker inspection must protect age/count retirement, forgotten intent, Catalog.Remove and physical deletion.
- Expected Release: faults before unlink and after unlink but before successful directory sync. Persist a complete ended marker, reopen and retry it. Assert a current different marker is untouched. Missing-marker and missing-directory retry must finish their respective directory durability barriers before reporting success.
- Completion drain: repeated successful server restarts and healthy adoption drain old ended claims while preserving the live claim. Cover same-process ended-save failure, failed write and lost acknowledgement on removal save, Release failure/retry and reload, and fence-blocked drain reaching its explicit next-holder call site.
- Fence: joining safety calls, cancelled waiters, release attempted while a stopper is paused, and the production-boundary Owner/Stop/Wait/retirement barrier. Keep exclusion through actual I/O completion, no directory reuse or replacement unpin, and no stale generation removal. A timeout does not detach unsafe work.
- Removal policy: preserve offline .forgotten success and assert pin preservation when the daemon restarts. Online catalog removal refuses physical removal while pinned. Ordinary sessions are unchanged. No CLI source change or marker-deletion tool is authorized.

### Commands for the implementation

Run on the host chosen through `machine-fleet`, with the pinned Go 1.27.3 toolchain and teardown installed first.

```bash
names='TestLaunchOwnership(StopsBeforeDelete|RecordedBeforeOriginStartReturns|EmptyIDFailure|ServerCreateFailure|FailedUpdateBeforeSwap|FailedUpdateAfterSwap|StopOutcomes|Identity|StopFailureRetainsAndRetries|RecordSaveFailure|EndedSaveFaults|RestartRecovery|SetupInterruptedRecovery|CancelledCaller|FencedSafetyStop|StaleRemovalKeepsNewerEntry|RecordedAndDiscoveredStopOnce|WholeRecordRestoreKeepsEntries|OneRunningEntryPerLabel|FoundUnrecordedReleasesMarker)'
test "$(go test -list "^${names}\$" ./internal/apps | grep -c '^TestLaunchOwnership')" -eq 20
test "$(go test -list '^Test(AppWorkersOwnerMarker|CatalogKeepsOwnedSessions)$' ./internal/daemon | grep -c '^Test')" -eq 2
test "$(go test -list '^TestLaunchWritesOwnerBeforeStart$' ./internal/worker | grep -c '^Test')" -eq 1
go test -race -count=1 -timeout=60s ./internal/apps -run '^TestAppPostLaunchFailureStopsKnownWorker$'
go test -race -count=1 -timeout=60s ./internal/daemon -run '^TestAppWorkersPreservePostLaunchIdentity$'
go test -race -count=1 -timeout=180s ./internal/apps -run "^${names}\$"
go test -race -count=1 -timeout=180s ./internal/daemon -run '^Test(Labelled|OwnedStop|DemandStopsAnUnpublished|AppWorkersOwnerMarker|CatalogKeepsOwnedSessions)'
go test -race -count=1 -timeout=60s ./internal/worker -run '^TestLaunchWritesOwnerBeforeStart$'
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/gates.sh
./scripts/verify.sh
git diff --check
git diff --name-only "$execution_base"
```

The two proof commands must pass with no edit to either proof file. The scope check must list only the paths in the scope table.

### STOP conditions for the implementation

- PR 324 is not merged, the managing limits below are exceeded, or the post-003 guard shows additional relevant drift beyond the reviewed Sync/expireUploads delta.
- Either proof test needs an edit to pass.
- The repair needs a change to `internal/protocol`, a new dependency, generic worker discovery, or any path outside the scope table.
- A test needs a real long-running worker outside the test's own scratch, live Mesh, owner data, or the installed daemon.
- Any regression shows an ordinary or another app's session stopped, an entry removed without a verified end or proven different marker, or a launch under a label with an unresolved running entry.
- Limitation 1 or 2 shows up in the implementation's own runs. Stop and report rather than adding a timeout or a "missing record means ended" rule.
- A gate fails twice after a reasonable correction.

### Post-003 drift guard

PR 324 merged at `807bd938f211ade8724bd80aded6283005f903a5`. Its origin.go is byte-identical to reviewed head `229589c8d23f82a336f922be7c044879484960ea`. The original design source remains c47b35ce. Its diff against `c47b35ce8a6e6e309bec9b99064230873ed86e69` touches `internal/apps/origin.go` in `Sync` and `expireUploads` only (18 added, 7 removed) and adds `internal/apps/origin_upload_cleanup_test.go`. None of this design's functions overlap it. Run after PR 324 merges and before the first production edit:

```bash
git fetch origin main
gh pr view 324 --json state,mergeCommit --jq '{state, merge: .mergeCommit.oid}'
git diff --stat c47b35ce8a6e6e309bec9b99064230873ed86e69..origin/main -- internal/apps/origin_worker_ownership_test.go internal/daemon/apps_ownership_test.go
git diff c47b35ce8a6e6e309bec9b99064230873ed86e69..origin/main -- internal/apps/origin.go internal/apps/core_test.go internal/daemon/apps.go internal/daemon/apps_test.go internal/daemon/lifecycle.go internal/daemon/demand_sessions.go internal/daemon/catalog.go internal/worker/launch.go internal/worker/worker.go internal/paths/paths.go .internal/plan/07-quality-and-security-wave.md .gates/baseline.json
```

Expect MERGED at the recorded merge, an empty main-side proof-path diff, and only the reviewed Sync/expireUploads delta in origin.go. Separately compare both imported proof blobs directly with 4c0121d9dbfed459724ce014494340e92798b8c3. Permit that known merged delta; stop on additional relevant changes in the listed functions or contracts. Record the actual future execution SHA and the merge SHA without rewriting historical design source identity.

### Original questions for the fresh review

1. Is the owner marker the smallest sound way to give a short reusable ID an incarnation and keep its exit evidence, given that `Start` and `Stop` must keep their signatures for the unchanged proof?
2. Is "a different marker proves a different incarnation" sound, given that only the origin releases markers and only after a durable `Ended`?
3. Is the fence acceptable while worker I/O cooperatively honors its budget, with no admission until every joined action really returns? No hard wall-clock maximum is promised for context-insensitive I/O.
4. Accept the explicit per-attempt orphan pin and potentially live pre-save workspace limitation, or fund stronger pre-launch recovery? Generic Forget must not blindly remove unknown markers. Any manual procedure first needs exact end and quiescence proof.

### Managing scope and product decision

Accepted November 1, 2026 in the coordinator conversation. This was the managing chat's decision, not an unanswered owner request.

1. Keep offline mesh rm unchanged. It records .forgotten and succeeds; physical removal of a pinned ended worker directory is deferred until app release. Online removal cannot discard the pin's evidence. No CLI source change is authorized.
2. Accept the launch-before-durable-save gap for this bounded repair. One crash may leave pins for several affected attempts, with no small byte cap, and an invisible pre-save worker may remain live or lose its workspace. Document exactly those limits and how an operator spots them in the existing Mesh plan or docs. Do not provide a manual marker-deletion tool or instructions to blindly unpin a directory.
3. Implement the corrected private marker, retention, exact-identity and safety-fence design. Stronger pre-launch ownership and a generic orphan-recovery redesign are outside scope. The implementation starts only after the verified 003 merge and fresh drift check.
4. Preserve the original failing-first proof, add its marked stop-before-delete companion and specified fault/concurrency coverage, obtain an independent code review, satisfy repository gates and merge. This local design branch remains unpublished until its red proof is made green in the implementation PR.

Operator observations must use actual app errors and session state: an app cleanup error naming an unresolved returned ID, a retained owner-marked session directory or launching marker, and deferred physical deletion after offline forgotten intent. A session command can be alive even if absent from the catalog. Marker presence alone does not prove a live process or a safe removable directory. Never advise rerunning a recipe while ownership is unresolved or deleting the marker without end and quiescence proof.

### Coordination evidence checked

`decisions.tsv` and `STATUS.md` were read as evidence only and not edited. Revision 1 flagged that STATUS described 003 as 23 production lines while PR 324 then held only its test. PR 324 now has pushed head `229589c8d23f82a336f922be7c044879484960ea` with the 18-added, 7-removed `origin.go` change and its test. This design read only that immutable head, not the author's uncommitted state, and did not repeat its gate diagnosis.

## Teardown receipt

The shell installs teardown before creating scratch or starting tests. Each named Go command runs in its own process group with a finite outer timeout. Cleanup reaps the owned commands, removes only the owned scratch, and verifies those process groups absent. Fixture cleanup logs confirm cleared synthetic workers, no origin gates, and the joined socket fixture goroutine.

The bounded batch exited 0 after validating the expected red test. A final adapter-only run passed after replacing unchecked deferred socket closes with an explicit ignored close result. `/work/tmp/m002-U0DImb` had 0 remaining logical bytes and was removed. Its four process groups were absent. The final adapter run removed `/work/tmp/m002-hpDqIO` with 0 remaining logical bytes and verified its process group absent. After the hook corrections, `final-validation/` repeated all four bounded commands with the same expected outcomes. It removed `/work/tmp/m002-dQEf6y` with 0 remaining logical bytes and verified all four owned process groups absent. The first before/after Mesh and Go process census matched exactly. The final adapter census briefly added Mesh PID 2431352, parent 2230, process group 2431352. That process was outside this run's owned group 2430880 and was preserved. It was already absent at the follow-up read. The heavy slot was released. Existing Mesh services and all shared caches were preserved.

Only the local three-file lane commit and concise proof evidence are retained for the coordinator's design handoff. The coordinator owns their removal after the green implementation absorbs the regressions and note. The earlier abandoned lane at `/home/shaul/.t3/worktrees/mesh/t3-5160db65` remains coordinator-owned and was untouched.

## Design-stage receipt

Revision 1 and revision 2 ran no Go test, build, formatter, integration script, or installed Mesh command. Each started from a tree whose proof files are byte-identical to `4c0121d9dbfed459724ce014494340e92798b8c3`, on base `c47b35ce8a6e6e309bec9b99064230873ed86e69`, so the recorded `final-validation/` statuses still describe the proof. Revision 2 read the review, source, and PR 324's pushed head `229589c8d23f82a336f922be7c044879484960ea` read-only. It did not read the 003 author's uncommitted state. Neither revision created a process, socket, scratch directory, or fixture. Outputs are this note in local commits and `/work/reports/wave-20261101/mesh-audit/002-design/`. The shared checkout, the 003 lane, the review lane, and the original proof worktree were not modified. The intentionally failing proof branch was not pushed or merged.

## Coordinator reconciliation receipt

The coordinator took over the completed design worktree only after Opus and both reviewers stopped writing. Only this note changed. Findings 1 to 3 were reconciled inside the reviewed marker architecture using exact-marker binding, durable reservation/release rules and one explicitly assigned ended drain. Finding 4 now states the online/offline behavior and honest per-attempt orphan and live-worker/workspace limits. Proof tests and all production files remain unchanged. No runtime result or independent APPROVE is claimed. The managing chat subsequently accepted both policy limits and authorized the bounded implementation under the decision recorded above. Production review and merge remain separate gates.


## Implementation execution, November 1, 2026

Execution base is `3d6b9bc27f687d92970cff5178ad2f0cb4c9fb79`. The post-003 guard contains exactly the reviewed Sync and expireUploads change. The original proof was imported as `e07f351d91a114a397f38e8d9b58e91537b75d7e`; both test blobs remain byte-identical to `4c0121d9dbfed459724ce014494340e92798b8c3`. This note was replaced from immutable approved `981287f6b7a3c233e4e3e4ff20af96589be1522e` before production edits.

The repeated initial app proof failed for its original live-worker/deleted-workspace assertion. The marked companion and bounded ownership, persistence-fault, recovery, safety-fence, catalog and real socket-boundary contracts pass in `002-implementation/contracts.log`. Their actual source identities and cleanup receipts are retained beside that log. Independent code review and full verification remain separate requirements.

The demand-session interface stays unchanged. Its unmarked start delegates to a private marked launcher shared with the app adapter. This carries the same approved marker through creationRequest and LaunchConfig without changing demand callers or expanding test scope.

The activating-unfound regression exposed a further ordering requirement inside the approved ensureServer path. It must stop and fully drain its known invisible server before probing that server's occupied replacement port. A valid discovered live server is adopted unchanged. Unknown ownership blocks both preflight and replacement.

Two full gate attempts stopped during pinned deadcode setup, before baseline comparison. The first retained offline proxy refusal. The second found symlinked module source in the shared cache, which Go embed rejects. The coordinator authorized a private regular-file pinned-tool cache, a bounded compilation control and one additional full run. Shared caches and tool versions stay unchanged. The baseline token permits only reductions proved by the real comparator.
