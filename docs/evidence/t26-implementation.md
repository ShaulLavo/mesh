# T26 implementation and verification

The implementation follows the [reviewed update plan](../tasks/T26-mesh-updates.md).
The [update guide](../updates.md) documents the command, fleet approval,
notifications, installation, and release process.

## Delivery boundary

Implemented source does not establish that a release was published or that a
host runs it. Publication requires the complete CI gate and native transition
proofs for Linux amd64, Linux arm64, and macOS arm64. Deployment requires a
persisted fleet operation and each target's verified executing-build receipt.
An offline, failed, or unsupported host remains visible in that operation.

The initial PC recovery fix was installed independently from a clean recovery
commit before the new updater was ready. Its daemon reopened the migrated
database, and six existing worker/server and shell PID pairs survived the
replacement. Those retained workers still ran older code. No missing terminal
checkpoint was reconstructed from data that had never been saved.

## Repeatable checks

```bash
go test -race ./...
go vet ./...
golangci-lint run ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
scripts/verify.sh
scripts/check-updates.sh
scripts/check-packaging.sh dist
```

Use a short `TMPDIR` for Unix-socket integration tests. Build, cache, and evidence
locations can be set with `GOCACHE`, `GOLANGCI_LINT_CACHE`, and
`MESH_UPDATE_CHECK_DIR`. This installation uses its mounted data SSD.

The local race suite, vet, lint, existing integration suite, and update
acceptance command passed during implementation. The vulnerability scan found
reachable SSH dependency defects; upgrading `golang.org/x/crypto` to v0.56.0
removed the reachable findings. Publication reruns the complete gate against
the exact committed source.

## Failure checks

The installer tests run real subprocesses and terminate the helper before and
after replacement and rollback. Reopening durable phases does not invent an
activation grant. Tests cover damaged artifacts, missing mounts, insufficient
space, managed installation paths, service definitions that would kill workers,
candidate health failure, actual database compatibility, and verified rollback.

The transaction retains the original executable inode through activation and
rollback. This matters on macOS: copying an old executable preserves its bytes
but does not preserve the mapped inode needed to verify a retained worker.
Native tests exercise replacement at the installed path and check the loaded
image, rather than only running old and new binaries at separate paths.

Coordinator tests cover signed requests, replay, target identity changes,
concurrent coordinators, shared installation receipts, dependency order, an
offline host returning to a pinned release, canary failure, lost acknowledgments,
explicit retry while offline, cancellation, and legacy bootstrap receipts.
The first coordinator's local approval survives loss of the initiating CLI,
including helper startup before its installation journal exists.

Notification tests cover cache timing, failed-check throttling, snooze, skipped
versions, pending operations, both pickers, detached refresh, and clean JSON or
session output. The command requires an explicit or saved fleet for unattended
whole-fleet approval.

## Native and reboot checks

`scripts/prove-release-transition.sh` starts each retained binary, creates a
real worker and shell, runs the candidate against retained state, reattaches and
exchanges terminal data, then rolls back and repeats the exchange. It checks
both process IDs and the candidate-written database. Content-addressed receipts
bind those results to the exact executable digests. The release workflow runs
this proof natively for all three platforms and all advertised baselines.

A disposable Linux VM exercises the unmodified CLI, daemon, independent helper,
and real user systemd services. Its release fixture uses HTTPS and a CA trusted
only inside the guest. A normal update preserved custom service definitions,
the original worker and shell processes, and terminal I/O.

An actual guest reboot exposed a startup race: the helper could observe the
daemon before its socket existed and consume the approved grant as a failure.
The corrected transaction waits for readiness and retains the grant for
autonomous retry. Boot identity also distinguishes service replacement from a
machine reboot: a known changed kernel boot permits installation to finish and
records interrupted sessions; an unknown or unchanged boot still requires the
original worker processes. Explicit retry retains that interruption evidence.

The repeated VM test passed: the helper resumed a granted update after a real
guest reboot, reached a verified commit without another command, and reported
the interrupted worker. Recovering its checkpoint created a new shell in the
saved directory and accepted terminal input. The original process memory was
not restored. `scripts/check-updates-vm.py --help` describes the reusable check
and its disposable-guest fixture requirements.

A separate fresh guest account started with only the CLI. One fleet approval
installed its daemon and helper, verified the local installation, and retained
an explicit offline member as pending in the same operation.

Subprocess tests establish journal behavior. VM checks establish the exercised
Linux service and reboot behavior. Neither substitutes for native macOS
publication proofs or per-host deployment receipts.
