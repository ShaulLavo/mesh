# Mesh SSH command benchmark

Status: Approved. This is the separate named speed benchmark for PR 435.
Correctness acceptance must not call it or assert command latency.

## Method and gate

`scripts/bench/ssh_command.py` runs the actual ordinary `mesh ssh HOST_ID -- command`
CLI against an owned Mesh listener and stock `ssh` against an owned OpenSSH
`sshd`. Both listeners bind only `127.0.0.2` on the same Linux host, run as the
same account, and use the same disposable Ed25519 key for host and client
authentication. Mesh pins the host identity; OpenSSH pins `known_hosts`.
The Mesh endpoint uses the existing integration-only fake Tailscale helper.
This measures ID-addressed ordinary command mode. Persistent attachment and
human-readable machine-name verification are outside this scenario.

Each invocation starts a fresh CLI, TCP connection and public-key authentication.
OpenSSH ignores user configuration and agents and disables multiplexing. Neither
client requests a PTY. Both execute `/bin/sh -c` with `/bin/cat` and a fixed stderr
marker, send the same 64 KiB binary input, and check every stdout byte, stderr
byte and exit status. Timing covers process launch through complete output and
client exit. OpenSSH's normal account-shell command dispatch remains included.
No synthetic sleep, mocked transport or persistent connection hides startup work.

Four warmup pairs precede 40 measured pairs. The order within each pair is
balanced and shuffled with a recorded seed. Warmups and measured raw samples,
order, per-command output hashes, distributions, command vectors, binary/runner
hashes, tool versions, OS, load, CPU count and filesystem go into JSON.
Warmups never enter the verdict. Partial samples survive a later command error.

A pair exceeds the margin only when Mesh takes **over 1.5 times** SSH's duration
and **over 50 ms extra**. Exit 1 requires at least 80% of measured pairs to exceed
both margins and a one-sided exact sign-test tail below 0.01 under the null that
at most half of pairs exceed them. Both comparisons are strict. These generous
margins ignore tiny absolute differences and isolated scheduler pauses. The
paired order limits drift; correlated host load remains a limitation, so inspect
raw distributions alongside failures. Thresholds are fixed, with no historical
calibration or timing ledger. Exit 0 means no clear slowdown by this rule;
exit 2 means a prerequisite, output, execution or cleanup error, never a speed
regression. The 30-second command cap only catches hangs.

## Parent interface and wiring

Required: Python 3.11+, Linux `/proc`, child subreaping and pidfds, existing
`ssh`, `ssh-keygen`, `sshd`, `/bin/sh`, `/bin/cat`, `df`, and an account that the
owned unprivileged OpenSSH daemon can authenticate with public keys. A platform
that disallows private sshd instances must supply a suitable Linux runner.
The parent supplies its already-built, reviewed `-tags mesh_integration` Mesh
artifact. This benchmark never compiles, downloads, installs, upgrades or changes
an owner service, credentials or SSH configuration. Configuration and keys are
created only under its private scratch root.

The outcome patch owner can add a separately named benchmark step using the
following interface; this patch deliberately leaves `ci.yml` unchanged:

```sh
# Inputs from the existing parent build and runner setup:
# MESH_INTEGRATION_BINARY: absolute executable artifact path
# MESH_SHORT_TMP: existing short scratch parent, e.g. /work/tmp on the PC
# BENCHMARK_REPORT: result JSON outside the disposable root
df -h "$MESH_SHORT_TMP"
python3 scripts/bench/test_ssh_command.py
python3 scripts/bench/ssh_command.py \
  --binary "$MESH_INTEGRATION_BINARY" --scratch-parent "$MESH_SHORT_TMP" \
  --output "$BENCHMARK_REPORT"
```

On the PC, run the benchmark with the installed heavy runner's `--class bench
--host local` admission. No hosted run or additional build is needed here.
Keep the JSON as the benchmark result even on nonzero exit. `--pairs` may increase
the sample count, with 20 as the minimum; it cannot alter the speed margins.

## Verification and cleanup

Full Mesh-versus-SSH result: **unmeasured**. No reviewed integration binary was
supplied to this implementation tree. The parent must run the command above
after composing its artifact; no timings from unit-test substitutes are claimed.

The real owned OpenSSH command/output/teardown test passed three consecutive
runs on the PC with its existing OpenSSH installation. Decision tests cover
clear slowdown, outliers, both margins, the 80% rule, confidence, output/status
errors and partial samples. Failure and SIGTERM tests verify descendant joining
and scratch deletion. Ruff checks pass.

Before setup the runner owns no endpoints or sessions. Each run owns one scratch
root, two listeners, two daemons and their command children. Existing integration
process-ownership helpers use subreaping and validated pidfds to stop and join
only descendants, including after failure or termination. The root is deleted
only after `waitpid` proves no child remains; failed cleanup retains and names
the root. JSON records logs and cleanup results. Normal `mesh ssh` creates no
persistent sessions, and the runner checks its isolated catalog and session tree.
The implementation tests left no listener, process or scratch root. No binary,
download, installation or fleet service was created or changed.
