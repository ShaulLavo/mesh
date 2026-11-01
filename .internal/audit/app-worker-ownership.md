# App worker ownership reproduction

Status: Approved reproduction stage, completed November 1, 2026. Production repair is not authorized by this note.

Design status: Pending Opus design and independent Sol review. Neither review has happened in this lane. The coordinator owns both gates and the later implementation handoff.

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

## Design gate and STOP conditions

The coordinator next obtains the Opus ownership design, followed by independent Sol review. This lane does not prescribe an ownership map, new interface, persistence model, or recovery architecture. The approved design must settle the transitions and crash behavior already listed in Plan 002 before a production executor starts.

Stop on ownership-changing source drift, an unexpectedly green app proof, a changed adapter contract, a need for live owner state, unbounded cleanup, or a repair that expands into generic discovery or public protocol changes. Retain the intentional failure. Do not skip or weaken it to make this branch mergeable.

## Teardown receipt

The shell installs teardown before creating scratch or starting tests. Each named Go command runs in its own process group with a finite outer timeout. Cleanup reaps the owned commands, removes only the owned scratch, and verifies those process groups absent. Fixture cleanup logs confirm cleared synthetic workers, no origin gates, and the joined socket fixture goroutine.

The bounded batch exited 0 after validating the expected red test. A final adapter-only run passed after replacing unchecked deferred socket closes with an explicit ignored close result. `/work/tmp/m002-U0DImb` had 0 remaining logical bytes and was removed. Its four process groups were absent. The final adapter run removed `/work/tmp/m002-hpDqIO` with 0 remaining logical bytes and verified its process group absent. After the hook corrections, `final-validation/` repeated all four bounded commands with the same expected outcomes. It removed `/work/tmp/m002-dQEf6y` with 0 remaining logical bytes and verified all four owned process groups absent. The first before/after Mesh and Go process census matched exactly. The final adapter census briefly added Mesh PID 2431352, parent 2230, process group 2431352. That process was outside this run's owned group 2430880 and was preserved. It was already absent at the follow-up read. The heavy slot was released. Existing Mesh services and all shared caches were preserved.

Only the local three-file lane commit and concise proof evidence are retained for the coordinator's design handoff. The coordinator owns their removal after the green implementation absorbs the regressions and note. The earlier abandoned lane at `/home/shaul/.t3/worktrees/mesh/t3-5160db65` remains coordinator-owned and was untouched.
