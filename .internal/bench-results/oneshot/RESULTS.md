# Fleet one-shot command benchmark — BEFORE baseline

Measured on 2026-11-23 at 07:01 UTC against unmodified `origin/main` Go production source (`aeae7393a7d7ff4eea97bab529df85a689aba46c`; checkout HEAD `0921769243e3e0229367edbd84dbb69a186f2cea` dirty only in `scripts/bench/`, Go 1.27.4). Raw samples, exact TCP connection mux proofs, and cleanup records are in [before.json](before.json).

## Method

- **Builds**: `CGO_ENABLED=0 go build -trimpath ./cmd/mesh` for `linux/amd64` (`c8122198a5cf…`), `linux/arm64` (`46eed9f1966b…`), and `darwin/arm64` (`3c02e7cb6164…`).
- **Isolation**: All remote orchestration (probing, staging via `scp`, triggering, teardown) uses a benchmark-owned OpenSSH ControlMaster socket pinned to each host's Tailscale IPv4 address (`-o HostName=<tailscale-ip>`) with read-only host-key verification and forwarding disabled (`UpdateHostKeys=no`, `StrictHostKeyChecking=yes`, `ClearAllForwardings=yes`, `ForwardAgent=no`, `ForwardX11=no`), with zero calls to `mesh ssh`, `mesh cp`, canonical daemons, or canonical Mesh state/config directories. Each target runs its own isolated `mesh daemon --tailnet-port <free> --ssh-port 0` with private `0700` `HOME`, `MESH_STATE_DIR`, `MESH_CONFIG_DIR`, exclusive `mkdir` + `.owner-token`, and fake `tailscale` status bound to that host's Tailscale IPv4 (`100.84.25.68` on `vps`, `100.94.222.118` on `pi`, `100.92.135.44` on `mac`, `127.0.0.1` on `local`). On `mac`, staging, daemon execution, server-side sampling, teardown, and any recovery all run inside `mac-queue.ts run`.
- **Modes measured**:
  1. **Warm OpenSSH (matched route, TCP mux proven)**: Explicitly owned OpenSSH ControlMaster (`ssh -N -M -o ControlMaster=yes -o ControlPersist=no`), verified via `-O check` (exact master PID match before and after sampling), `SSH_CONNECTION` destination IP check, exact established TCP socket inode identification from `/proc/<pid>/net/tcp` + `/proc/<pid>/fd` (checked on every sample and asserted unchanged after sampling), and `-o ProxyCommand=false` on client invocations so silent fallback to a fresh connection fails immediately. Runs `cd <scratch>/work && exec /bin/true` (or `/usr/bin/true` on macOS) over the exact same Tailscale IPv4 address as Mesh (`100.84.25.68`, `100.94.222.118`, `100.92.135.44`; owned loopback `sshd` on `127.0.0.2` for `local`).
  2. **Mesh MCP one-shot (black box)**: Persistent `mesh mcp --identity bench` stdio process on the PC, `mesh_context_open` (`shell: "none"`, `cwd: <scratch>/work`), then timed `mesh_exec` (`argv: ["/bin/true"]` or `["/usr/bin/true"]`, `wait_ms: 10000`) timed end-to-end as a black box including initial output retrieval.
  3. **Mesh server-side**: Local client on the target host sending two fresh Unix-socket RPCs (`execution.create` then `execution.wait`, always sending `execution.wait` even if `execution.create` returned a terminal receipt) over the isolated `daemon.sock` (3 warm-up + 30 measured).
  4. **Network RTT**: `tailscale ping -c 5 --until-direct=false <tailscale-ip>` from the PC.
- **Pairing and verdict**: Balanced random interleaved pairs (`seed=435`), 3 warm-up pairs dropped, 30 measured pairs per host. Every sample verifies exit code 0 and completed execution. Promise policy reuses `scripts/bench/ssh_command.py` `summarize`: `slowdown` if >= 80% of pairs exceed both `1.5x` and `+50 ms` with one-sided sign test `p < 0.01`, `no clear slowdown` otherwise when `n >= 7`, or `inconclusive` when too few pairs are collected to reach `p < 0.01`. Speed is reported, never asserted.

## Load and environment notes

- The PC (`local`, 28 logical CPUs) was under load from other concurrent jobs during this run (load average `19.55, 16.20, 22.64` at start; `11.03` during `local`; `7.40, 11.54, 18.61` at end). Local timings reflect that contention and are not a quiet-machine reading.
- Remote host load averages at probe time: `vps` `0.49`, `pi` `0.95`, `mac` `3.38` (AC power, 100% charged).
- Matched-route and TCP mux proofs passed on all four hosts (`network_matched: true`, `tcp_inode_unchanged: true`, single established TCP inode verified before, during, and after sampling on every host).

## Results

| Host | RTT p50 (ms) | Warm SSH p50/p90 (min–max) ms | Mesh MCP p50/p90 (min–max) ms | Ratio (p50) | Delta (p50) | Server create/wait/total p50 (ms) | Promise | <= SSH p50 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- |
| local | local | 5.0 / 6.2 (3.8–6.5) | 2734.5 / 5379.7 (846.7–8045.3) | 547.46x | +2729.5 ms | 1796.4 / 481.0 / 2595.5 | slowdown | no |
| vps | 81.0 | 175.6 / 181.0 (171.9–200.3) | 521.6 / 569.5 (448.4–713.4) | 2.97x | +346.0 ms | 206.5 / 5.5 / 212.9 | slowdown | no |
| pi | 7.0 | 26.0 / 47.5 (16.7–61.0) | 491.0 / 872.5 (297.7–1939.3) | 18.85x | +465.0 ms | 316.0 / 5.7 / 333.7 | slowdown | no |
| mac | 6.0 | 37.2 / 39.1 (29.6–47.8) | 517.7 / 571.6 (462.9–869.9) | 13.90x | +480.5 ms | 348.8 / 69.2 / 420.0 | slowdown | no |

### Server-side distribution (`execution.create` + `execution.wait` on target host)

| Host | Create p50/p90 (min–max) ms | Wait p50/p90 (min–max) ms | Total p50/p90 (min–max) ms |
| --- | ---: | ---: | ---: |
| local | 1796.4 / 3387.8 (721.2–4964.0) | 481.0 / 896.0 (172.6–1423.8) | 2595.5 / 4317.1 (893.8–5596.9) |
| vps | 206.5 / 273.1 (133.0–330.1) | 5.5 / 24.8 (3.0–41.0) | 212.9 / 299.2 (156.9–359.7) |
| pi | 316.0 / 465.0 (224.3–776.1) | 5.7 / 34.3 (3.3–165.5) | 333.7 / 569.2 (253.0–779.7) |
| mac | 348.8 / 656.8 (303.3–2465.7) | 69.2 / 119.6 (1.6–1578.2) | 420.0 / 943.2 (375.6–2531.8) |

## Cleanup verification

All four hosts (`pc`, `vps`, `pi`, `mac`) reported `clean: true`, `remaining_pids: []`, and `scratch_removed: true` in `before.json`, and independent live OpenSSH checks confirmed no remaining scratch directories, no remaining benchmark processes, and `mac-queue.ts` lock `free`.
