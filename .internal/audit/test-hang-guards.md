# Test hang guard follow-up

Status: Approved. Follow-up composed on merged PR 470, `6d032d9b75e2e0f66dc43ea5c88d6f7a9501ad5e`.
Production timing, retries, installer behavior, acceptance assertions and baselines
are unchanged. The five harness references advance to the composed source so CI
executes these helper changes.

## Evidence

Run 43820103589, AMD0, verified the actual v0.1.211 worker, shell, protocol 1,
and digest `2c91334eae167b91c1b66ed6725121c75b162f0b0090c21ff1a75f5f5b4fc741`.
Its failed-image control then hit create-session's 3-second socket deadline
before the permission failure could arm. The control failed after 3.06 seconds.
Retained input: `/work/reports/mesh-auto-update/activation-current-main/amd0-43820103589/v0.1.211.worker.log`.
The sibling `amd0-43820103589.log` records the job. This patch keeps the identity,
metadata, failure injection, worker and shell liveness, and cleanup assertions.

## Inventory and changes

Search areas: first-party Go `*_test.go`, `internal/testenv`,
`integration/helpers`, `scripts/prove*`, `scripts/test*`, and `scripts/fixtures`.
Inspect `WithTimeout`, `After`, `NewTimer`, socket deadlines, subprocess timeouts,
monotonic deadline loops, and fixed poll counts with sleeps. Vendored tests and
benchmarks are outside this follow-up.

| Shared helper | Finding and action |
| --- | --- |
| `internal/testenv` | Environment and portable socket roots existed, but no shared hang cap. Add `HangTimeout`, two minutes. |
| `updatebootstrap/legacy_integration_test.go` | Raise create, readiness, and process-list cleanup from 3 seconds. One cleanup context covers the initial listing and every nested `ps` poll. |
| `daemon/runtime_test.go`, `run_fixture_test.go` | Raise the shared 3-second runtime cap. Preserve deliberate timeout controls through a fixture-local override. |
| `worker/worker_test.go`, `cli/daemon_test.go` | Raise shared condition and server-completion guards from 2 seconds and 1 second. |
| `cli/catalog_setup_boundary_test.go` | The related race suite reproduced a healthy setup expiry under its 1-second guard. Raise healthy setup and reply guards. Preserve the barrier-triggered 50-millisecond expiry input and every authority/cache assertion. |
| `integration/helpers/mesh_control.py`, `terminal_window.py` | Raise shared socket and PTY defaults to 120 seconds. Worker cleanup consumes its existing absolute deadline for connect, reply, and socket removal. |
| `scripts/fixtures/native-helper`, `tls-probe`, `tls.py` | Raise helper preparation, TLS handshake, certificate generation, probe execution, and join guards. Python calls still respect the installed fixture's parent clock. |
| `scripts/prove-legacy-release-reader.py` | Replace the 120-poll readiness guard with a two-minute deadline. Bound each CLI query by its remainder. Bound post-KILL reap by one cleanup deadline. |
| `scripts/prove-release-transition.sh` | Replace shared readiness, token, exit, session-publication, and post-stop socket poll caps with two-minute deadlines. Stop escalation and socket polling share one stop deadline. |
| `scripts/test-verify-runner.py` | Raise shared barrier waits and descendant reconciliation to two minutes. Keep runner cancellation stimuli and outcome assertions. |
| `scripts/fixtures/installed_clock.py`, `release_workers.py`, `interrupt_trace.py` | Existing minute-scale work and cleanup guards already exist. Keep them. |

## Intentional short values retained

- Daemon fixture controls deliberately withhold readiness or completion for 3 seconds.
  Their containing subprocess gets the generous hang guard.
- `worker_operations_test.go` uses 20 milliseconds to exercise operation expiry.
  Catalog, authentication, cancellation, demand readiness, and idle tests also
  inject short product timers. These are stimuli, not scheduling requirements.
- `scripts/test-verify-runner.py` injects short cancellation stages through
  `TIMINGS`. Its signal-order, ownership, sentinel, and cleanup checks remain.
- Poll intervals, server accept ticks, and repeated observation slices remain short.
  They do not end a successful control while its outer deadline remains available.
- PTY fixture cleanup retains `DEFAULTS["grace"] - 5`, currently 25 seconds,
  because its containing owner kills descendants after 30 seconds. TERM-to-KILL
  escalation remains 2 seconds there and 120 polls of 50 milliseconds in the
  transition proof. Post-KILL waits consume the shared remaining cleanup allowance.
- Listener and attachment diagnostic collection has short best-effort limits.
  Missing diagnostics preserve the original failure and do not reject success.

## Larger follow-up

This is a shared-helper patch, not a rewrite of every test. Package-local helpers
still carry independent short guards: CLI catalog/picker and attachment helpers,
daemon relay/server and session-inspector helpers, fleetshare waits, update
configuration/adoption helpers, and worker channel/read helpers. Specialized
integration and proof callers also pass explicit 3-to-15-second overrides.
Review those by purpose before extending this change. In particular,
`scripts/prove-device-auth-cutover.py`, `prove-helper-recovery.py`, and
`fixtures/release_transition.py` mix hang guards with deliberate service faults.
Do not substitute durations throughout them or alter retry counts or backoff.

The full installed acceptance transaction and fleet tests are not run on the live PC.
Local direct-daemon controls use private state, an unavailable user bus, and owned
process cleanup. Local builds are sufficient for these checks.

## Verification

The hosted retry then failed before artifact intake while listing producer jobs
through GitHub's API. Its captured error omitted `gh` stderr, so the original
HTTP status is unknown. A contemporaneous read of the same endpoint returned
HTTP 502. The helper now retries only HTTP 500, 502, 503 and 504, at most three
times within one two-minute deadline. Permanent API errors, command timeouts and
invalid success JSON still fail; final errors include the endpoint and bounded,
redacted details. The actual-command 502 control fails the old helper and passes
the fix. All 16 API and artifact controls passed three times on Python 3.12 and
3.14. No extra hosted rerun was started.

The review correction applies the shared guard to terminal back output, device
identity and approval, name adoption, and the non-TTY entry check. Normal runner
completion now shares its two-minute observation guard; explicit cancellation
controls retain their injected stage bounds. A real six-second command covers
the former back-output cutoff. The specialized clipboard campaign still has a
containing-owner deadline and needs its own lifecycle audit before changing it.
Composition preserves the zero-byte reset classification and helper receipt
revalidation from PR 470. Its newer ordered-handoff controls also use two-minute
process, barrier and join guards, with owned helper lifetimes longer than that
guard. Synthetic expiration inputs and exact identity assertions are unchanged.
The six-second control fails against the original helper with `TimeoutExpired`.
It and the runner's real success and failure-aggregation controls pass three
times on each of Python 3.12 and 3.14. Repository-configured Ruff and diff checks
pass. The subprocess is direct, and its timeout path kills and reaps it before
temporary state is removed; no task processes remain.

Use the explicit local runner with Go 1.27.4 and the default online module proxy.
Logs are under `/work/reports/mesh-test-hang-guards-20261116-9hAzla`.

- `go test -race` passed for `internal/testenv`, `internal/updatebootstrap`,
  `internal/daemon`, `internal/worker`, and both Go fixture tools.
- The first full CLI race run found the healthy catalog setup's 1-second expiry.
  After the guard fix, that control passed with `-race -count=3`, including its
  barrier-triggered expired-setup case.
- Daemon cleanup controls and legacy process-list cancellation passed with
  `-race -count=3`. The real v0.1.211 identity and injected image-failure cleanup
  test also passed three times against the cached original executable.
- Three runs each passed: `scripts/test-release-transition.py` with 16 tests,
  `scripts/test-fixture-tls.py` with 12 tests, `scripts/test-verify-runner.py` with
  39 tests, `integration/fixture_cleanup.sh`, and real-worker diagnostic controls
  in `integration/helpers/worker_failure_test.py` with four tests.
- The full CLI race suite passed after the fix with `-timeout=20m`, a package hang
  cap. Full `scripts/gates.sh` passed with zero new or stale findings, and
  `go mod tidy -diff` was clean. PTY cleanup and real-worker diagnostics passed
  again after the import-only lint correction. The existing baseline is unchanged.

Cleanup was checked on the PC after the consumers stopped. No process command
line still referenced the task scratch root, and that root was removed. Only the
small review logs remain in the report directory, removable after root's review.
