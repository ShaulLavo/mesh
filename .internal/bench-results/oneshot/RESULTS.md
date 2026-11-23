# Fleet one-shot command benchmark — BEFORE baseline

Measured on 2026-11-23 at 06:26 UTC against unmodified `origin/main` Go production source (`aeae7393a7d7ff4eea97bab529df85a689aba46c`; checkout HEAD `4a7ed8e5d6e923b290aa5f6d8999ca3ba746fd28` dirty only in `scripts/bench/` and `.internal/bench-results/oneshot/`, Go 1.27.4). Raw samples, mux proofs, and cleanup records are in [before.json](before.json).

## Method

- **Builds**: `CGO_ENABLED=0 go build -trimpath ./cmd/mesh` for `linux/amd64` (`2451d40ba373…`), `linux/arm64` (`868c43d98563…`), and `darwin/arm64` (`8288f5bfa14d…`).
- **Isolation**: All remote orchestration (probing, staging via `scp`, triggering, teardown) uses a benchmark-owned OpenSSH ControlMaster socket pinned to each host's Tailscale IPv4 address (`-o HostName=<tailscale-ip>`), with zero calls to `mesh ssh`, `mesh cp`, canonical daemons, or canonical Mesh state/config directories. Each target runs its own isolated `mesh daemon --tailnet-port <free> --ssh-port 0` with private `0700` `HOME`, `MESH_STATE_DIR`, `MESH_CONFIG_DIR`, and fake `tailscale` status bound to that host's Tailscale IPv4 (`100.84.25.68` on `vps`, `100.94.222.118` on `pi`, `100.92.135.44` on `mac`, `127.0.0.1` on `local`). On `mac`, staging, daemon execution, server-side sampling, teardown, and any recovery all run inside `mac-queue.ts run`.
- **Modes measured**:
  1. **Warm OpenSSH (matched route, mux proven)**: Explicitly owned OpenSSH ControlMaster (`ssh -N -M -o ControlMaster=yes -o ControlPersist=no`), verified via `-O check` (exact master PID match before and after sampling), `SSH_CONNECTION` destination IP check, per-sample master TCP socket inode check, and `-o ProxyCommand=false` on client invocations so silent fallback to a fresh connection fails immediately. Runs `cd <scratch>/work && exec /bin/true` (or `/usr/bin/true` on macOS) over the exact same Tailscale IPv4 address as Mesh (`100.84.25.68`, `100.94.222.118`, `100.92.135.44`; owned loopback `sshd` on `127.0.0.2` for `local`).
  2. **Mesh MCP one-shot (black box)**: Persistent `mesh mcp --identity bench` stdio process on the PC, `mesh_context_open` (`shell: "none"`, `cwd: <scratch>/work`), then timed `mesh_exec` (`argv: ["/bin/true"]` or `["/usr/bin/true"]`, `wait_ms: 10000`) timed end-to-end as a black box including initial output retrieval.
  3. **Mesh server-side**: Local client on the target host sending two fresh Unix-socket RPCs (`execution.create` then `execution.wait`, always sending `execution.wait` even if `execution.create` returned a terminal receipt) over the isolated `daemon.sock` (3 warm-up + 30 measured).
  4. **Network RTT**: `tailscale ping -c 5 --until-direct=false <tailscale-ip>` from the PC.
- **Pairing and verdict**: Balanced random interleaved pairs (`seed=435`), 3 warm-up pairs dropped, 30 measured pairs per host. Every sample verifies exit code 0 and completed execution. Promise policy reuses `scripts/bench/ssh_command.py` `summarize`: `slowdown` if >= 80% of pairs exceed both `1.5x` and `+50 ms` with one-sided sign test `p < 0.01`, `no clear slowdown` otherwise when `n >= 7`, or `inconclusive` when too few pairs are collected to reach `p < 0.01`. Speed is reported, never asserted.

## Load and environment notes

- The PC (`local`, 28 logical CPUs) was under heavy load from other concurrent builds and test suites during this run (load average `37.00, 34.62, 35.31` at start; `25.35` during `local`; `19.40, 27.43, 32.49` at end). Local timings reflect that contention and are not a quiet-machine reading.
- Remote host load averages at probe time: `vps` `0.44`, `pi` `0.79`, `mac` `3.06` (AC power, 100% charged).
- Matched-route verification passed on all four hosts (`network_matched: true`; OpenSSH `SSH_CONNECTION` destination IP matched Mesh's Tailscale IPv4 on `vps`, `pi`, and `mac`, and loopback on `local`).

## Results

| Host | RTT p50 (ms) | Warm SSH p50/p90 (min–max) ms | Mesh MCP p50/p90 (min–max) ms | Ratio (p50) | Delta (p50) | Server create/wait/total p50 (ms) | Promise | <= SSH p50 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- |
| local | local | 6.0 / 8.3 (4.7–21.4) | 1940.3 / 4349.8 (965.9–8566.8) | 323.53x | +1934.3 ms | 1032.2 / 246.3 / 1330.2 | slowdown | no |
| vps | 81.0 | 175.8 / 179.9 (172.4–211.7) | 508.5 / 890.5 (430.8–1052.0) | 2.89x | +332.7 ms | 175.4 / 3.9 / 179.7 | slowdown | no |
| pi | 3.0 | 18.8 / 38.6 (14.8–99.0) | 423.7 / 638.5 (374.1–1272.3) | 22.50x | +404.9 ms | 416.2 / 33.1 / 445.9 | slowdown | no |
| mac | 6.0 | 36.5 / 40.4 (30.0–43.9) | 690.9 / 963.5 (561.8–2383.5) | 18.91x | +654.3 ms | 409.9 / 66.2 / 482.8 | slowdown | no |

### Server-side distribution (`execution.create` + `execution.wait` on target host)

| Host | Create p50/p90 (min–max) ms | Wait p50/p90 (min–max) ms | Total p50/p90 (min–max) ms |
| --- | ---: | ---: | ---: |
| local | 1032.2 / 3033.9 (572.5–4704.3) | 246.3 / 691.2 (143.3–1415.9) | 1330.2 / 3725.1 (758.7–6120.2) |
| vps | 175.4 / 212.1 (132.0–257.7) | 3.9 / 9.3 (2.0–14.8) | 179.7 / 217.3 (145.5–259.8) |
| pi | 416.2 / 605.4 (269.3–799.1) | 33.1 / 81.8 (3.3–553.5) | 445.9 / 627.9 (314.0–852.5) |
| mac | 409.9 / 548.3 (347.9–621.1) | 66.2 / 92.6 (1.4–96.9) | 482.8 / 555.3 (431.8–686.1) |

## Cleanup verification

All four hosts (`pc`, `vps`, `pi`, `mac`) reported `clean: true`, `remaining_pids: []`, and `scratch_removed: true` in `before.json`, and independent live OpenSSH checks confirmed no remaining scratch directories, no remaining benchmark processes, and `mac-queue.ts` lock `free`.
