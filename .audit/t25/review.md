# T25 review

The review covered recovery claims and receipts, native launch context, hook
setup, picker behavior, and the SSH startup change. Each finding below was
reproduced with a failing test before its fix.

## Findings

- An unverified replacement lost access to its exact pending conversation after
  a second worker crash. Recovery must reuse the frozen launch plan without
  representing it as an acknowledged provider identity. The picker must retain
  the corresponding conversation action.
- The pending resume plan prevented the fallback shell from registering a new
  agent. New foreground invocations must remain capturable, while only a matching
  native resume invocation can confirm the original recovery.
- Claude backend switches and a custom API endpoint were accepted for automatic
  capture even though the recipe did not preserve them. Capture must reject that
  context and let the native invocation run with its original environment.
- Installing provider hooks replaced a symlinked settings file, leaving its
  tracked target unchanged. Setup must atomically update the target and preserve
  the link.
- A closed local agent hid a later saved remote target in the picker. The default
  remote recovery action must retain its target label and directory context.
- `agent doctor` treated any `agent-hook` text as installed configuration. It now
  checks for the selected provider's actual startup command at the current Mesh
  executable path.

## Checks beyond the changed functions

The pinned Wish constructor initializes the same underlying Charm SSH server
and installs a host key. Mesh still installs its configured signer and reasserts
its authentication callbacks after extension options. Existing authentication
tests, the session panic test, and per-IP rate-limit tests passed. The production
dependency check rejects legacy terminal libraries that query a PTY before main.

The regression tests call the real launch parser, worker registration handlers,
recovery transaction, settings writer, and picker rendering. Original source
records and unverified pending plans must remain distinct from resume receipts.
Read-only native lookup must never supply such a receipt.

Final validation passed: all 35 integration scripts, `go test -race ./...`,
`go vet ./...`, `scripts/check-t25.sh`, Linux amd64/arm64 and Darwin arm64
cross-builds, module consistency, formatting, and whitespace checks. Unlimited
lint reports the same eleven pre-existing findings. Logs are under
`/work/tmp/mesh-t25-review`.

Native compatibility evidence remains in
[the retained report](../../docs/agent-recovery-compatibility.md). These fixes do
not expand the tested provider versions or claim VM power-loss validation.
