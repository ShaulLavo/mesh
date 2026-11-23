# Fleet one-shot command benchmark — BEFORE baseline

Measured on 2026-11-23 at 05:47 UTC against `origin/main` commit `aeae7393a7d7ff4eea97bab529df85a689aba46c` (Go 1.27.4, unmodified production source; worktree dirty only from the new benchmark scripts). Raw samples and full metadata are in [before.json](before.json).

## Method

- **Builds**: `CGO_ENABLED=0 go build -trimpath ./cmd/mesh` for `linux/amd64` (`8bdbce1f2007…`), `linux/arm64` (`d0bdca263350…`), and `darwin/arm64` (`31e31ba92d1e…`).
- **Isolation**: Each target runs its own scratch `mesh daemon --tailnet-port <free> --ssh-port 0` with private `HOME`, `MESH_STATE_DIR`, `MESH_CONFIG_DIR` (`0700`), and fake `tailscale` status bound to that host's tailnet IPv4 address (`100.84.25.68` on `vps`, `100.94.222.118` on `pi`, `100.92.135.44` on `mac`, `127.0.0.1` on `local`). On `mac`, the remote daemon and server-side phase run inside `mac-queue.ts run`. No canonical daemons, state directories, or identities are touched.
- **Modes measured**:
  1. **Warm OpenSSH**: `ssh -o BatchMode=yes -o ControlMaster=auto -o ControlPath=<private>/c -o ControlPersist=60 -T HOST true` against OS SSH aliases `vps`, `raspberry-pi`, and `shaul-mac` (and an owned `sshd` on `127.0.0.2` for `local`), master warmed before sampling.
  2. **Mesh MCP one-shot**: persistent `mesh mcp --identity bench` stdio process on PC, `mesh_context_open` (`shell: "none"`), then timed `mesh_exec` (`argv: ["/bin/true"]` on Linux, `["/usr/bin/true"]` on macOS, `wait_ms: 10000`).
  3. **Mesh server-side**: local client on the target host sending keyed pipe `execution.create` + `execution.wait` over the isolated `daemon.sock` (3 warm-up + 30 measured).
  4. **Network RTT**: `tailscale ping -c 5 --until-direct=false <tailscale-ip>` from the PC.
- **Pairing and verdict**: Balanced random interleaved pairs (`seed=435`), 3 warm-up pairs dropped, 30 measured pairs per host. Every sample verifies zero exit status and completed execution. Promise policy reuses `scripts/bench/ssh_command.py` `summarize` (`slowdown` if >= 80% of pairs exceed both `1.5x` and `+50 ms` with one-sided sign test `p < 0.01`). Speed is reported, never asserted.

## Load and environment notes

- The PC (`local`, 28 logical CPUs) was under heavy load from other concurrent agent builds and test suites throughout this run (load average `48.21, 46.28, 33.59` at start; `44.01` during `local`; `29.98, 40.33, 38.50` at end). Local timings reflect that contention and are not a quiet-machine reading.
- Remote host load averages at probe time: `vps` `0.97`, `pi` `2.83`, `mac` `3.85` (AC power, 100% charged).
- Note on SSH transport paths from the PC: `vps` uses Tailscale (`100.84.25.68`), while OS SSH aliases `raspberry-pi` and `shaul-mac` route over ZeroTier (`10.168.63.43`, `10.168.63.32`), and Mesh routes over Tailscale (`100.94.222.118`, `100.92.135.44`).

## Results

| Host | RTT p50 (ms) | Warm SSH p50/p90 (min–max) ms | Mesh MCP p50/p90 (min–max) ms | Ratio (p50) | Delta (p50) | Server create/wait/total p50 (ms) | Promise | <= SSH p50 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- |
| local | local | 8.1 / 24.7 (4.1–252.8) | 3600.1 / 7317.1 (1160.6–12246.4) | 447.08x | +3592.0 ms | 1968.3 / 484.6 / 2519.6 | slowdown | no |
| vps | 81.0 | 179.7 / 186.5 (172.2–204.4) | 500.7 / 794.5 (422.8–915.8) | 2.79x | +321.0 ms | 178.5 / 4.0 / 183.0 | slowdown | no |
| pi | 4.0 | 19.7 / 42.6 (13.3–56.6) | 642.4 / 1743.6 (396.2–2442.1) | 32.66x | +622.7 ms | 363.4 / 41.1 / 392.0 | slowdown | no |
| mac | 5.0 | 34.0 / 45.2 (28.9–267.7) | 609.3 / 864.8 (479.7–1170.6) | 17.90x | +575.2 ms | 408.4 / 65.6 / 483.3 | slowdown | no |

### Server-side distribution (`execution.create` + `execution.wait` on target host)

| Host | Create p50/p90 (min–max) ms | Wait p50/p90 (min–max) ms | Total p50/p90 (min–max) ms |
| --- | ---: | ---: | ---: |
| local | 1968.3 / 3633.7 (680.2–6625.5) | 484.6 / 619.7 (155.7–1877.5) | 2519.6 / 4529.7 (904.6–7144.0) |
| vps | 178.5 / 206.0 (140.0–242.2) | 4.0 / 6.9 (2.6–32.8) | 183.0 / 211.7 (149.8–247.5) |
| pi | 363.4 / 418.0 (267.0–506.5) | 41.1 / 55.5 (3.3–1323.5) | 392.0 / 433.1 (315.5–1680.3) |
| mac | 408.4 / 521.8 (332.8–552.1) | 65.6 / 89.7 (1.6–95.8) | 483.3 / 544.2 (396.9–553.8) |

## Cleanup verification

All four hosts (`pc`, `vps`, `pi`, `mac`) reported `clean: true`, `remaining_pids: []`, and `scratch_removed: true` in `before.json`, confirmed independently via live host checks (no remaining scratch directories, no remaining benchmark processes, and `mac-queue.ts` lock free).
