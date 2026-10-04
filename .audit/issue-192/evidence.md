# Issue 192 evidence

Status Approved. This PR reduces repeated claim-cache read barriers in `LoadHosts`.
It preserves durable admission on authenticated connections.

## Cause and scope

The current issue comment describes the shipped changed-writer correction in
`af0861954d4c557d16c9d7623d6acffc48c1f095`. That correction remains in place.
The source at `ebe4b11797521dc55c8298b12d053ea6a012534e` still opens the cache,
settles its ancestry, reads a record and settles its publication once per host.

`CachedClaims` now opens one private anchored cache directory per call. It reads
and validates the requested records, then settles the final directory barrier.
Every observed replacement precedes this barrier, including replacements whose
publisher previously failed its directory sync. A failure returns no claims.
Missing records remain unnamed. No observation survives in memory between calls.
The obsolete single-record production API is removed. Reader fixtures exercise
the batch API with one owner through test helpers.

`LoadHosts` batches the canonical identity owners, then projects their claims.
Legacy configured IDs remain supported. Address-book contents, projection rules,
writer admission, identity/name revisions and authenticated consumer verification
are unchanged. This batch is a set of observations, not an atomic snapshot across
independent owners.

## Regression and concurrency

The first commit adds an unoptimized batch API and the regression. The original
per-record implementation fails with this output in `red.txt`:

```
batch publication barriers=3, want 1 after all records
```

The optimized implementation passes. The tests also exercise a failed publication
followed by a failed batch barrier, recovery through a successful batch barrier,
invalid owners, corrupt records, missing records, duplicate requests and concurrent
real filesystem publications. Two writers publish revisions 2 through 16 while
32 batches read their records. Each owner's observations must retain its identity
and increase monotonically; the final batch must see both revision-16 records.
The existing containment, ancestor-creation, replay/equivocation and writer tests
remain enabled.

## Availability disposition

The authenticated loopback fixture holds a real exclusive flock after committing
its initial claim. An exact-ID request and a bare-name request both fail cache
admission with `context deadline exceeded`. Neither reaches a destination list
operation. After releasing the lock, the exact-ID request succeeds and reaches
one operation. The production 250 ms lock bound and 5 ms poll interval remain
unchanged. `green.txt` records the bounded fixture observations.

This refusal is required by the current durable authority contract. An unchanged
visible record may still require a failed-publication barrier; a destination claim
must also pass replay and equivocation checks before effects. Exact-ID selection
does not remove those checks. Bypassing cache failure would weaken authenticated
consumer freshness. External lock contention and storage failures can therefore
still refuse a verified connection. This PR makes no availability improvement or
universal latency claim. It finishes the investigation with that disposition and
reduces the independently measured read cost.

## Measurement

The committed `BenchmarkLoadHostsClaimCache` uses private scratch configuration,
canonical fixture IDs and real `RememberClaim` writes. It opens no network endpoint.
The writer case runs up to eight independent writers repeating their unchanged
claims while the real `LoadHosts` path reads them. Changed concurrent publication
is covered separately by the race test above.

Both runs use Linux amd64, Go 1.27.0 and the `/work` filesystem. The shared slot
wrapper drains finite jobs for `bench --quiet`; it reports two existing servers,
which this lane does not inspect or operate. Each case has five samples of twenty
scans. Setup and fixture publication are outside the timed interval. These are
medians of five per-sample ns/op results, not per-request percentile estimates.
The raw outputs are `before.txt` and `after.txt`.

```
export PATH=$HOME/.local/share/mise/shims:$PATH
bun /work/platform-production/heavy/current/run.js --class bench --quiet "issue192 baseline" -- env PATH="$PATH" TMPDIR="$PWD/.audit/issue-192" go test ./internal/cli -run '^$' -bench '^BenchmarkLoadHostsClaimCache$' -benchtime=20x -count=5
bun /work/platform-production/heavy/current/run.js --class bench --quiet "issue192 improved" -- env PATH="$PATH" TMPDIR="$PWD/.audit/issue-192" go test ./internal/cli -run '^$' -bench '^BenchmarkLoadHostsClaimCache$' -benchtime=20x -count=5
```

The baseline used the original `LoadHosts` implementation plus the benchmark from
commit `2ddcd9e`. The after run uses this PR's batch reader with the same workload.
No claim is made about native ARM storage timing, other filesystems, render counts
or frontend performance.

Run `python3 .audit/issue-192/compare.py` to recompute the sample medians and
reductions from the raw files. `comparison.md` contains its output. At 100 hosts,
median scan cost falls from 4.090724 to 0.753267 ms without writers and from
11.033641 to 1.540199 ms with writers. The corresponding reductions are 81.6%
and 86.0%. Ten-host medians fall by 72.5% and 76.4%. Single-host short samples
include scheduler variation; they do not establish a systematic improvement.

## Verification

The production batching change passed `go mod tidy -diff`, `go vet ./...`,
`go test -race ./...` across 44 packages and `./scripts/verify.sh` across all 62
integration scripts. The first quality scan reported only two new unreachable
symbols, the obsolete single-record API and its sync helper. That report is
retained in `gates-before-cleanup.txt`. The production API was removed and the
existing single-record fixture assertions were migrated to one-owner batches.
A final affected-package race run and full quality scan verify that cleanup.
No baseline entries, lock deadlines or existing assertions were weakened.

The post-migration full affected-package race run passed both
`./internal/machinename` and `./internal/cli`. The final naming/cache race run
passed after adding error context to the CLI fixture helper. Full quality gates
report zero new and zero stale findings for golangci, deadcode, ShellCheck and
Ruff. The final gate and focused output are `gates.txt` and `green.txt`.

```
bun /work/platform-production/heavy/current/run.js --class suite "issue192 required checks" -- env PATH="$PATH" bash .audit/issue-192/check.sh
bun /work/platform-production/heavy/current/run.js --class suite "issue192 migrated reader checks" -- env PATH="$PATH" bash .audit/issue-192/recheck.sh
bun /work/platform-production/heavy/current/run.js --class suite "issue192 final gate and fixtures" -- env PATH="$PATH" bash .audit/issue-192/final-check.sh
```

The check scripts create and clean their own scratch directory under `/work/tmp`.
The full race and integration outputs are `race.txt` and `integration.txt`; the
post-migration complete affected-package race output is `affected-race.txt`.
The final focused command is `go test -race ./internal/machinename ./internal/cli
-run 'ClaimCache|Name|Rename' -count=1 -v`.
