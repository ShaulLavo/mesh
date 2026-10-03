# Development

Run `./scripts/gates.sh` for formatting, vet, golangci-lint, deadcode, ShellCheck,
Ruff, and the bootstrap and terminal dependency contracts. Go analysis targets
Linux/amd64 with CGO disabled so the checked-in baseline is independent of the
machine running it. The baseline checker uses only the Go standard library.
Python 3 is needed for the existing release tools and the CI-only planted-violation proof.

From the repository root, install the pinned tools and hooks once:

```bash
mise install
lefthook install
```

`mise.toml` pins Go 1.27.0, golangci-lint 2.13.2, ShellCheck 0.11.0, Lefthook
2.1.15, and Ruff 0.16.9. Use `mise exec -- lefthook install` if your shell does
not activate mise. CI verifies the ShellCheck, Ruff, and Lefthook release checksums.
Lefthook is a development tool, not a Go module dependency.

Pre-commit runs `gates.sh --fast`. It checks gofmt on staged Go files, vet and
golangci findings in their packages, and ShellCheck on staged shell files.
It inspects an isolated snapshot of the Git index, so unstaged changes cannot
hide staged violations or block an unrelated commit. Only Go, golangci-lint,
ShellCheck, and Lefthook are needed for this hook. Missing or mismatched tools
fail with an install hint. Full scans alone enforce removal of stale baseline
entries because partial package graphs can omit cross-package findings.

Lefthook installs hooks in the common Git directory. The same installation
covers linked worktrees. `assert_lefthook_installed` makes a missing Lefthook
executable fail rather than silently skip checks. Each hook resolves its config and `scripts/gates.sh`
from the worktree where Git runs it. Pre-push runs the full gates command and
also requires Ruff. Full gates run in CI. Deadcode runs through
`go run golang.org/x/tools/cmd/deadcode@v0.49.0` without changing `go.mod`.

`.gates/baseline.json` records existing findings with reasons and occurrence counts.
Go lint keys use file, rule, normalized diagnostic, and enclosing Go function, not a
source-line fingerprint. Diagnostic line numbers, complexity scores, and contextcheck
SSA closure ordinals are omitted; callee paths and literal digits remain significant.
Growth of an already-baselined complexity score is not ratcheted; new findings and
per-key finding-count growth still fail. Goconst uses only the literal within its file,
so a representative occurrence or reported count can change without churning the key.
ShellCheck and Ruff retain source-statement keys, preventing a fixed exception from
covering an unrelated replacement warning. Goconst, gocognit, and dupl exclude
`_test.go`; all security linters still check tests. Dupl's exclusion filters report
location, not analyzer input, so a production-side report of a test/production clone
still fails unless baselined.
New findings fail. Fixed findings also fail until you run
`./scripts/gates.sh --update-baseline`. That command removes entries or reduces
counts, but refuses all additions without changing the baseline. Any new exception
needs a deliberate manual edit and a reason. `--report-dir DIR` retains raw tool
reports for inspection. `third_party/` is excluded from quality scans. Its SSH
patch contracts remain in the integration suite.

Run `./scripts/verify.sh` for the integration suite. It builds both test binaries
once and runs up to four tests concurrently, limited by available CPUs. Set
`MESH_INTEGRATION_JOBS` to choose a different concurrency limit. Each script gets
a scratch HOME and an allow-listed environment. The verifier resolves Go and
Python before replacing HOME. It preserves user-bus discovery so Linux scope
checks cannot silently skip coverage, plus `MESH_TEST_ZSH` and `MESH_SHORT_TMP`.
The initial binary builds keep effective Go module policy, proxy settings, CA
trust, and an absolute netrc path for dependency downloads. Those build-only
settings do not reach tests. Fixtures set any provider or proxy settings they
need. Standalone agent scripts also scrub inherited settings and pin Python
through their shared fixture. Use the verifier to isolate the full shell-script
environment. See the
[implementation status](plan/02-status.md) and [task briefs](tasks/)
for the design and build order.
