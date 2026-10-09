# Quality and security wave

Status: Approved, 2026-09-30. Not started. Written to be handed to a wave coordinator.

## Outcome

Mesh gets the same guard rails Fregat has, a codebase cleaned by independent audits, and a
hardened attack surface checked by two different models. Every finding is fixed or explicitly
parked with a reason. After the wave:
- One `gates` command blocks new dead code, duplication, deep nesting, unwrapped errors and
  lint findings, in pre-commit and CI.
- Every area of `internal/` has been audited for quality by one Sol run and one Opus run.
- Every attack surface has been audited for security by at least two Sol runs and one Opus run.
- Everything above is fixed, released, and running on every host in the fleet, temporary apps
  (plan [06](06-temporary-apps.md), T29) included.

## How to run it

Run it with the `orchestrate` skill (`~/.agents/skills/orchestrate`), which sits on poteto mode.
The coordinator plans, steers design calls, merges and releases. Agents implement and review.

- **Models:** Sol (GPT 6.1 Sol, native `sol` subagent) does most audits, implementation and
  reviews. Opus subagents take the second audit pass, taste-heavy refactors, and any unit Sol
  failed twice. Check usage with `bun ~/.agents/skills/orchestrate/scripts/usage.ts` before each
  launch batch.
- **Two eyes:** every audit area gets runs from both models, launched independently with the
  same brief and without seeing each other's output. Every security fix is reviewed by the other
  model from the one that wrote it.
- **Pace:** at most 4 live agents unless the owner raises it. Launch in small batches; bursts
  hit rate limits.
- **Machine:** heavy commands (`go test -race ./...`, `scripts/verify.sh`, builds) go through
  `/work/tmp/wave-heavy/run.sh`. Do not run test-heavy lanes while a Fregat input-latency
  calibration is running on this host: CPU load corrupts its measurements. Audits are read-only
  and may run alongside it.
- **Ledger:** `/work/reports/mesh-wave/` holds the checklist, events log, audit reports,
  findings table and handoff.
- **Every brief restates:** `CLAUDE.md`'s six invariants, `docs/plan/01-decisions.md`, "all
  integration scripts stay green", "no `internal/protocol` rename", and "no new dependency
  without a line of justification".

This wave's own agents run inside Mesh sessions on this host. A daemon upgrade on this host is a
live test of invariant 3. Update the other hosts first; this host goes last, with the rollback
binary in place.

## Phase 0: baseline

1. Main CI green (`ci.yml`: vet, race tests, `verify.sh`, golangci-lint, govulncheck).
2. Record the baseline in the ledger: head SHA, test counts, `verify.sh` duration, golangci
   findings count, govulncheck output, and binary size.
3. Read `docs/plan/00-overview.md`, `01-decisions.md`, `02-status.md` and the task briefs of any
   area a lane will touch.

## Phase 1: toolchain gates

One lane (Sol, high). Fregat's `bun run gates` is the model: one command, run in pre-commit and
CI, with allow-lists that carry a reason for every entry.

| Fregat gate | Mesh equivalent |
|---|---|
| knip (unused code) | `deadcode` (golang.org/x/tools/cmd/deadcode) over `./cmd/...`, plus golangci `unused` and `unparam` |
| dupes, dupes:functions | golangci `dupl`, plus `goconst` |
| never-nester (max nesting 3) | golangci `nestif` and `gocognit` |
| errors:census | golangci `errorlint`, `wrapcheck` and `nilerr`, matching `CLAUDE.md`'s error rule |
| oxlint | golangci `gocritic`, `misspell`, `revive` (curated rules), `exhaustive`, `contextcheck` |
| (none) | `shellcheck` for `scripts/` and `integration/`, `ruff` for the Python release tools |

- **Baseline, then ratchet.** Existing violations go in a checked-in baseline so the gate blocks
  new ones from day one. Phase 4 burns the baseline down. A baseline entry is never added to
  make a new change pass.
- **Wiring:** `scripts/gates.sh` runs everything; lefthook runs it pre-commit; `ci.yml` gets a
  gates job. `third_party/` is excluded and gets its own drift check (Phase 3).
- **The `check-tNN.sh` scripts** (T16–T25) are audited in Phase 2: fold any that still protect
  behavior into `integration/` or the gates, and delete the rest.
- **Acceptance:** the gate fails on a planted violation of each kind, passes on main, and runs
  in CI. It gets one independent review.

## Phase 2: quality audits

Read-only. Each area gets one Sol run and one Opus run with the `improve` skill, each producing
ranked, self-contained fix plans in `/work/reports/mesh-wave/quality/<area>-<model>.md`.

| Area | Paths |
|---|---|
| CLI and entry | `internal/cli` (`command.go` is the largest file), `cmd/mesh` |
| Daemon | `internal/daemon` (demand, runtime, lifecycle, relay), `internal/procmem` |
| Sessions and terminals | `internal/worker`, `session`, `terminal`, `protocol`, `transport`, `inspection` |
| TUI | `internal/tui` |
| Serving and edge | `internal/edge`, `serve`, `dnsname`, `webauth` |
| Temporary apps | `internal/apps`, `internal/apppill`, `examples/` |
| SSH | `internal/sshd`, `sshfs`, `tunnel` |
| Host setup and updates | `internal/bootstrap`, `identity`, `tailnet`, `update`, `updatebootstrap`, `updategate`, `updateinstall`, `updatenotice`, `release` |
| Recovery, power and state | `internal/recovery`, `agentresume`, `wake`, `wakeclient`, `inhibit`, `storage`, `paths`, `db/` |
| Scripts and release | `scripts/`, `integration/`, `.github/workflows`, goreleaser, Casks |

The coordinator merges both runs into one findings table: area, finding, severity, which models
found it, and fix unit. A finding both models raised ranks first. A finding one model raised is
checked by the coordinator before it becomes a unit.

## Phase 3: security audits

Read-only. The first step is a short threat model (`/work/reports/mesh-wave/security/threat-model.md`):
assets, trust boundaries, attackers (tailnet peer, public internet, local user, compromised
agent, malicious release), and the invariants security depends on. Each surface then gets at
least two Sol runs (different prompts: one attacker-led, one code-led) and one Opus run.

| Surface | What to check |
|---|---|
| **Temporary apps (first)** | Shipped in v0.1.55 and runs arbitrary code behind a public URL: process isolation from the host and other apps, filesystem reach of app workspaces, owner recognition (Tailnet ownership, paired grants, browser pairing), public/private switching, the injected pill as a script-injection point, name reservation and guessing, expiry that really stops processes and deletes files, resource limits per app |
| Public edge and serving | Authentication, request limits and deadlines, header and host handling, TLS and certificate installation, private DNS, serve-on-demand start-up as an amplifier |
| SSH front door | Auth, sessions over SSH, SFTP/SCP path containment and read-only guarantees, named reverse tunnels and claim lifetime |
| Transport | WebSocket auth and resume, replay ring bounds, frame limits, relay backpressure, protocol parsing of hostile frames |
| Local daemon and workers | Socket permissions, cross-user access, attach/steal authorization, signal and kill scope, `meta.json` trust, containment paths |
| Agent hooks and recovery | Hook identity (the 2026-09-25 hijack: inherited `MESH_AGENT_*` env let child processes overwrite saved ids), resume targets, env leakage into child sessions |
| Resource limits | Per-session memory/CPU scopes (the 2026-09-25 OOM killed all of `mesh.service`), hibernation, queue bounds, disk growth of logs and replay |
| Host setup | Auth-key and sudo-password paths, remote output handling, identity verification, installers run as root |
| Updates and supply chain | Release signing and checksum verification in `mesh update` and `install.sh`, GitHub API trust, rollback, pinned actions, govulncheck, `third_party/` (wish, sftp) drift from upstream and patch review |
| Pi wake and power | Target-owned permission, wake protocol spoofing, inhibitor scope |

Findings go in `/work/reports/mesh-wave/security/findings.md` with severity (critical, high,
medium, low), exploit scenario, affected invariant, and fix unit. **Critical and high findings
are fixed before any quality refactor lands in the same area.** A critical finding is reported to
the owner the same turn.

## Phase 4: fixes

- The coordinator turns findings into units, each with a plan: data shape, owning package,
  contracts touched, invariants at risk, acceptance, and the test or integration script that
  fails first.
- Order: critical and high security, then gate baseline burn-down, then quality refactors by
  value, then medium and low security.
- Each unit: an implementer in its own worktree (`/work/worktrees/mesh/<unit>`), one
  independent reviewer (the other model for security fixes), coordinator checks the fixes, merge
  on green CI.
- Large-file refactors (`command.go`, `tui/model.go`, `daemon/demand.go`) are split by
  responsibility, not by line count, and keep behavior identical: existing integration scripts
  are the safety net, plus new ones where coverage is thin.
- Update `02-status.md` and the task briefs as units land, as the pickup rules require.

## Phase 5: release and rollout

- Release through the existing pipeline (`scripts/plan-release.sh`, goreleaser,
  `check-packaging.sh`, Casks).
- Update order: `shaul` (Linux), then `mac`, then this host (`omarchy`), each with
  `mesh update --host`. Before updating this host, keep the previous binary as the rollback and
  confirm live sessions survive (`mesh ls` before and after).
- After each host: `mesh doctor`, sessions attached, serving routes answering, logs clean.

## Phase 6: backlog

- T29 leftovers: production rollout checks and real Safari-device verification of the pill,
  after the temporary-apps security fixes land.
- Agent compatibility work listed in `02-status.md` (other provider versions, a managed
  standalone Codex daemon) stays outside this wave.
- A draft "agent command execution" plan exists only as an uncommitted file in the shared
  `/work/projects/mesh` checkout. It is not part of this wave and needs owner approval first.

## Historical fixture failures

Approved follow-up, 2026-10-08. Plan 336 Track H transfers these reports into this execution home. Reviewed current main `9b7a47cfc05e349e42e816cc72720a1787d138f3` and every issue comment. The existing diagnostics and passing controls are useful evidence; none establishes the causes of these historical failures.

| Report | Retained observation and current evidence | Next bounded action |
|---|---|---|
| [#76](https://github.com/ShaulLavo/mesh/issues/76) | `shell_recovery.sh` and `serve_on_demand.sh` reached worker publication timeout. Original worker evidence was deleted. Six alternating instrumented full suites passed; zsh was unavailable. PR #66 shipped phase/incarnation diagnostics. | On genuine recurrence, retain worker log, metadata, socket, launch marker and process identity before cleanup. Identify the failed publication phase before writing a regression. |
| [#77](https://github.com/ShaulLavo/mesh/issues/77) | A suite-owned worker remained after state deletion and was absent at the next census 149 seconds later. Its creator and exit time are unknown. PR #123 fixed a separately reproduced signal-fixture cleanup leak. | Use fixture ownership receipts to attribute the actual worker and descendants at the next recurrence. Prove cleanup waits for that fixture's processes; preserve unrelated sessions. Do not identify the old PID with the separately fixed fixture. |
| [#78](https://github.com/ShaulLavo/mesh/issues/78) | Profile children received SIGKILL, including later `allocs` / `allocs-close` cases. Attribution stayed unexplained, owned sends empty and available OOM deltas zero. PR #71 added attribution instrumentation; a bounded Go 1.27.0 success control passed. | Retain child incarnation, executable/compiler provenance, owned sends, cgroup and kernel context on recurrence. Unknown sender remains unknown; passing controls and absent OOM counters are not attribution. |
| [#115](https://github.com/ShaulLavo/mesh/issues/115) | `release_transition_checkpoint.sh` reported child-start `fork/exec /bin/sh: operation not permitted` at `2a5056d`. Twelve concurrent controls and a syscall-observed known-good launch passed. PR #142 reports numeric errno and selected child path with `stage=unavailable`. | Preserve the failing child's pre-exec syscall entry/exit sequence on recurrence. Compare setsid, TIOCSCTTY and execve with the known-good control. Sanitize paths/arguments; do not infer the failing syscall from Go's errno. |
| [#194](https://github.com/ShaulLavo/mesh/issues/194) | PR #179 correction run on `503720a` had blank output in the fourth ordinary window, failing `MESH_PROMPT> ` at 4s. A quiet control and three default-concurrency controls passed. | Retain PTY/client startup output and actual worker selection/incarnation before cleanup. Reproduce `integration/helpers/terminal_window.py` fourth-window seam without changing the prompt assertion. |
| [#195](https://github.com/ShaulLavo/mesh/issues/195) | The same run's `temporary_app_server.sh` failed creating an app-setup worker, `workerAlive=false`, `startupLog=child-start-failed` after 5s. Its original worker log was deleted. Three default-concurrency controls passed. | Retain the app-setup worker log and child errno before cleanup, then use the isolated temporary-app fixture. Coordinate diagnostic retention with Fregat Plan 302. Do not blame the unrelated cache change. |
| [#197](https://github.com/ShaulLavo/mesh/issues/197) | Unmodified `a207f0e` control 2 displayed an interrupted-session picker, then missed a fresh prompt at 4s. Output was nonempty, unlike #194. The worker log was removed by normal cleanup. | Preserve relaunch selection, worker identity/startup and PTY receipts at `relaunch_interrupted()` on recurrence. Keep freshness and retained-process identity checks. |
| [#238](https://github.com/ShaulLavo/mesh/issues/238) | `TestAttachConcurrentWriterPrefillsLargePipe` on `ff65d8d` failed `F_SETPIPE_SZ` requesting 128 KiB before attach. Later unchanged capacity probes and focused test passed. Current test still requires enlargement. | Capture owned pipe capacity and user pipe-page availability on recurrence, with a known-good capacity control. Distinguish fixture admission from production attach. User-page pressure is a hypothesis, not a measured cause. |
| [#252](https://github.com/ShaulLavo/mesh/issues/252) | macOS 15 CI cancellation `after_acknowledgement` passed in 120.26s, then the CLI package hit 180s. The one-second post-cancellation assertion passed. Unchanged retry and first merged-main native run passed; that main case took 0.04s. | On recurrence, retain stage timestamps or a native execution trace across setup, stack inspection, cancellation and cleanup. The inspected wake bound was 110s. Preserve cancellation, relay-leak and no-worker-signal assertions. |
| [2026-10-09 recovery fixture](#recovery-races-child-start-failure-2026-10-09) | One local full suite failed the `race-source` worker's child startup on `facabc5`, Go 1.27.2. An isolated run passed with an earlier Go 1.27.1 integration binary and a different temporary path. The child errno was removed by fixture cleanup. | Retain worker evidence, then compare two bounded isolated runs with the same Go 1.27.2 normal binary and different temporary paths. Binary, toolchain, path, and load effects remain unresolved. |

- [ ] Keep each row as an independent pending investigation. The linked issue bodies/comments retain exact original commands, source hashes and evidence locations. Read them before reproducing; no common cause is established.
- [ ] Establish that observation works on a known-good owned fixture before interpreting a missing event. Retain failure receipts before fixture cleanup, with argument/environment privacy.
- [ ] At a genuine recurrence, attribute the failing operation and add its narrow fail-first regression. Use the approved heavy-job runner and owned state. Do not raise deadlines, weaken assertions, skip cases, change host settings or broadly retry to make a gate green.
- [ ] Do not restart the original broad campaigns merely to close tracker entries. Existing bounded controls reached their stopping points. Native investigations run only on an authorized available host; this closeout makes no Mac execution claim.
- [ ] Record the demonstrated cause, repair and focused verification beside the owning row when it is completed. Until then, issue closure means execution ownership transferred here, never that the bug was fixed or disproved.

### Recovery races child-start failure, 2026-10-09

Approved follow-up. Source: `facabc511852ae66a08bacae2f61dabfe40fd6d6`, the private-app branch later delivered through [PR #285](https://github.com/ShaulLavo/mesh/pull/285). One local `scripts/verify.sh` run failed `integration/recovery_races.sh`; every other integration passed. The suite used Go 1.27.2 and built fresh normal and `mesh_integration` binaries. `recovery_races.sh` received the normal binary. The outer integration deadline was 60s; the reported 5s deadline belongs to worker readiness.

The recorded invocation ran from `/work/worktrees/mesh/disconnect-app-pill`:

```bash
GOCACHE=/work/cache/go-build GOMODCACHE=/work/cache/go/pkg/mod \
GOTMPDIR=/work/tmp/mesh-pill-check/scratch TMPDIR=/work/tmp/mesh-pill-check/scratch \
MISE_DATA_DIR=/work/cache/mise MISE_CACHE_DIR=/work/cache/mise/cache \
MISE_DOWNLOADS_DIR=/work/cache/mise/downloads MESH_INTEGRATION_TIMEOUT=60s \
bun /work/platform-production/heavy/current/run.js --class suite \
  mesh-private-complete-integrations -- scripts/verify.sh
```

The heavy-job receipt in `/work/platform-production/heavy-jobs/2026-10-09.jsonl` ended at `2026-10-09T08:29:12.003Z`, exit 1, after 230038ms. The retained failure was:

```text
FAIL: create failed: {'type': 'error', 'requestId': 'race-source', 'message': 'daemon: session.create: launch worker WE3D: readiness (see /work/tmp/mesh-pill-check/scratch/mesh-recovery-_6s_0goo/remote/s/WE3D/worker.log): worker is still publishing state (elapsed=5.006441284s deadline=2026-10-09T08:28:14.014907002Z phase=launch-marker-present workerPID=1987186 workerAlive=false metaState=unknown startupLog=child-start-failed)'}
```

The immediate isolated control used `MESH=/work/tmp/mesh-pill-check/mesh-integration bash integration/recovery_races.sh` through the suite-class heavy runner. Its receipt, `mesh-recovery-races-recheck`, ended at `2026-10-09T08:31:41.369Z`, exit 0, after 976ms. That executable was an earlier Go 1.27.1 build with the `mesh_integration` tag, and the control used the default temporary path. It does not isolate the full suite's binary, toolchain, temporary path, or concurrency. PR #285's [Go 1.27.2 CI race and full integration checks](https://github.com/ShaulLavo/mesh/actions/runs/37904070707/job/113733182166) also passed. No cause is established by these passing controls.

The fixture removed its temporary directory and worker log. At `integration/helpers/recovery_transactions.py:238`, the exception handler prints terminal and daemon output but omits individual worker logs before cleanup. `internal/worker/memory.go:50` preserves numeric child errno when available; `startupLog=child-start-failed` alone does not retain that errno or identify a failing syscall.

First bounded follow-up:

1. In an isolated checkout, retain `remote/s/*/worker.log`, metadata, launch markers, owned process identity, and fixture startup timestamps before `fixture.close()` and temporary-directory cleanup. Sanitize arguments and environment values. Confirm this capture on a known-good owned fixture.
2. Use source `facabc511852ae66a08bacae2f61dabfe40fd6d6` and the Go 1.27.2 executable at `/work/cache/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.2.linux-amd64/bin/go`. Build one normal binary under an owned `/work/tmp` evidence directory through the heavy runner. Record `go version -m` and its SHA-256. Use that same binary for both commands below; do not substitute the older tagged control binary.

   ```bash
   recovery_evidence=$(mktemp -d /work/tmp/mesh-recovery-races-XXXXXX)
   export PATH="/work/cache/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.2.linux-amd64/bin:$PATH"
   export GOCACHE=/work/cache/go-build GOMODCACHE=/work/cache/go/pkg/mod
   bun /work/platform-production/heavy/current/run.js --class build \
     recovery-races-control-build -- go build -o "$recovery_evidence/mesh" ./cmd/mesh
   go version -m "$recovery_evidence/mesh"
   sha256sum "$recovery_evidence/mesh"
   ```

3. Run one isolated control for each path, with `GOCACHE=/work/cache/go-build`, `GOMODCACHE=/work/cache/go/pkg/mod`, and the pinned Go toolchain on `PATH`:

   ```bash
   TMPDIR=/work/tmp/mesh-pill-check/scratch GOTMPDIR=/work/tmp/mesh-pill-check/scratch \
   MESH="$recovery_evidence/mesh" bun /work/platform-production/heavy/current/run.js \
     --class suite recovery-races-same-path -- timeout 60s bash integration/recovery_races.sh
   TMPDIR=/tmp GOTMPDIR=/work/tmp/mesh-pill-check/scratch \
   MESH="$recovery_evidence/mesh" bun /work/platform-production/heavy/current/run.js \
     --class suite recovery-races-short-path -- timeout 60s bash integration/recovery_races.sh
   ```

4. On recurrence, read the retained child-start errno and path first. Distinguish child launch failure from a worker that remains alive but cannot publish metadata or a socket. Use the owned process and resource receipts to assess load, process limits, and path effects. Attribute a pre-exec syscall only with a retained syscall trace. Neither the source path nor the load is a demonstrated cause yet.

Keep this item open until a bounded reproduction establishes the cause and a focused regression proves its repair. Preserve readiness and recovery assertions and their deadlines. No new issue or broad reproduction campaign is required for this recorded follow-up.

## Done when

- Gates run in pre-commit and CI; the baseline is empty or every remaining entry has a reason.
- Every area has two quality reports and every surface has at least three security reports,
  all merged into the findings tables.
- Every critical and high finding is fixed with a test that fails without the fix; the rest are
  fixed or parked with a reason in the findings table.
- Main CI green, released, all three hosts updated, and the handoff lists what was parked.
