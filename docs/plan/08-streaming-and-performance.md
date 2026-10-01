# Streaming state and performance groundwork

Status: Approved, 2026-10-01. The owner set the order: performance work and the
streaming data path land first; the [terminal fleet dashboard](07-dashboard.md)
resumes on top of them. Plan 07 is paused until step 4 below is verified.

## Outcome

Every Mesh client reads fleet state the cheap way. A viewer such as the TV
dashboard holds one control connection per host and receives changes as they
happen, instead of opening a WebSocket, verifying identity and decoding a full
catalog every few seconds. An idle host with no viewers does almost nothing: no
per-second SQLite writes, no sampling, no preview rendering. The Raspberry Pi,
which both drives the TV and serves as a low-resource test host, has headroom
left for its other jobs.

## Findings at origin/main (v0.1.103)

Read-only inspection, 2026-10-01. Line references are at that revision.

| Finding | Evidence | Cost |
| --- | --- | --- |
| Reconcile writes SQLite every second even when nothing changed | `daemon/app.go:31,636`; `storage/store.go:213-254` upserts host `LastSeenAt` and every session per pass | Constant WAL writes (SD-card wear on the Pi), CPU and fsync per second per host |
| `session.list` carries a recovery preview per session | `daemon/recovery.go:79-105`, up to 32 KiB per session (`protocol/recovery.go:20`) | List payloads grow with session count; every reader JSON-decodes screen text it may not show |
| Each read opens a new connection and re-verifies identity | `cli/remote.go:90`; picker every 2 s (`tui/catalog_refresh.go:13`), inspector 2 s, summaries 4 s | One TCP+WebSocket handshake, two goroutines and a `host.info` round trip per host per read |
| Read paths can wake hosts | `CollectHostSessions` → `DialHost` with `recoverHost` (`cli/remote.go:22-26`, `cli/wake.go:87`) | A viewer can power a sleeping machine just to look at it |
| `mesh ls` scans all of `/proc` locally on every call | `procmem.Snapshot()` (`cli/command.go:1135,1893`) | Full process scan and `smaps_rollup` reads per invocation |
| Every mesh process hashes its own executable at startup | `release/current.go:14,25-45`, ~35 MB SHA-256 | Startup CPU on every CLI call, worse on the Pi |
| Tailnet and Unix listeners have no connection cap | `daemon/runtime.go:323` caps only the public listener | Unbounded goroutines and relay queues (8 MiB each way, `relay.go:15-19`) |
| `mesh.service` has no resource accounting or limits | `scripts/install/assets/mesh.service` | No measurement of the daemon's own footprint; workers share the unit's cgroup (`KillMode=process`) |
| Dashboard view types hold unbounded lists | `DashboardHostView.Sessions/Services` (feat/dashboard worktree) | Memory grows with the fleet |

The dashboard worktree (`feat/dashboard`) is based on local `main` at `492ae74`,
114 commits behind `origin/main`. Rebase it before resuming plan 07.

## Design

### 1. Change-driven reconcile

Reconcile keeps its one-second cadence for detecting change, but computes a
diff against the previous pass in memory. SQLite is written only for sessions
whose stored fields changed, in one transaction per pass that has changes.
Host liveness (`LastSeenAt`) moves to memory and is persisted at most once a
minute, or on shutdown. The same diff feeds the watch publisher (§3), so there
is one source of change, not two.

Gate: on an idle host with five running sessions, SQLite writes drop from
about 60 per minute to at most 1 per minute (count WAL frames or `sqlite3_changes`
over ten minutes), with identical `mesh ls` output.

### 2. Lean catalogs

`session.list` gains an additive request option to omit recovery previews and
other heavy fields. The picker and inspector keep previews by asking for them
explicitly (`session.inspect` for the rows they show). Old clients that send no
option keep today's full response. `mesh ls` caches its local `procmem`
snapshot for its own run only and reads remote memory from the daemon's
existing 10-second sampler instead of rescanning.

Gate: a 20-session `session.list` lean payload is measured and recorded against
the full one; the picker shows identical previews.

### 3. Watch protocol

Add one additive control, `state.watch`, carried on an ordinary control
connection opened with `DialControl` (`transport.DialOnce`): it never wakes a
host. The client verifies `host.info` first, then subscribes:

```text
state.watch { topics: [sessions, services, metrics], metricsEvery: 2s }
  → state.snapshot { seq, sessions[lean], services, metrics? }
  → state.event    { seq, kind: session.added|session.removed|session.changed|
                     service.changed|metrics, payload }
  → state.resync   { reason }   // client must expect a new snapshot
```

- **Single source.** Session and service events come from the reconcile diff (§1).
  Metrics come from an `internal/hostmetrics` sampler that this plan delivers
  (step 3): CPU, RAM, temperature and uptime, as specified in plan 07's
  measurement contract. Start from the salvaged dashboard work
  (`origin/wip/dashboard-salvage`, 829af08), which already contains a draft
  sampler and the `host.metrics` request; review it rather than trusting it.
  The sampler runs only while at least one subscriber asked for metrics, at the
  slowest requested interval within bounds. With no subscribers nothing
  samples. This keeps plan 07's "no always-on sampling loop" rule.
- **Bounded queues.** Each subscriber has a fixed-size queue. Pending events
  coalesce by key (the latest state of a session replaces an older unsent one).
  On overflow the publisher drops the queue and sends `state.resync`, then a
  fresh snapshot. A slow viewer can never grow the daemon's memory.
- **Sequence numbers.** Events carry a per-connection sequence. A gap or a
  reconnect means the client takes a new snapshot; it never guesses.
- **Liveness.** The existing 15-second transport keepalive detects dead viewers.
  A viewer that stops reading is dropped, not buffered.
- **Freshness without change.** An unchanged catalog sends no events, so the
  publisher sends `state.current { seq, reconciledAt }` every ten seconds while
  subscribed. It costs a few bytes and tells the viewer how old the host's
  latest reconcile is. A viewer marks a host's catalog stale when no event or
  `state.current` arrived for thirty seconds, matching plan 07's catalog
  staleness rule, and shows the age from `reconciledAt` translated to its own
  monotonic clock.
- **Compatibility.** A daemon that answers `unknown control` gets the plan 07
  polling path, unchanged, as a fallback, cached per host identity and build.
- **Invariants preserved.** Sessions belong to their host; the watch carries
  control data only, never terminal bytes; the Pi is still an ordinary client.

`mesh ls`, the picker's catalog refresh and the dashboard all become watch
clients where it saves work. A one-shot `mesh ls` still uses the lean list,
because a single read doesn't need a subscription.

Gates:
- Idle traffic per subscribed host under 1 KB/s once the snapshot is sent.
- A session state change reaches a viewer within one reconcile interval plus
  network latency.
- A stalled viewer causes a resync, never daemon memory growth (soak with a
  viewer that stops reading).
- Zero wake calls from any watch path (counted through the real CLI wiring).

### 4. Bounds and footprint

- Cap tailnet and Unix control connections and watch subscribers per daemon,
  with clear refusal errors. The caps are settings in the daemon config.
- Hash the executable lazily: only the paths that need build identity
  (update, compatibility checks) compute it, once, and cache it per process.
- Measure the daemon's own memory with systemd accounting
  (`MemoryAccounting=yes` in `mesh.service`). Do not put `MemoryMax` on the unit
  while workers live in its cgroup: a cap there could kill sessions. If a cap
  is wanted, workers move into their own scopes first, as a separate step.

Gates: startup time for `mesh --version` and `mesh ls` measured before and after
on the Pi; the daemon's idle RSS and CPU on the Pi recorded over an hour with
zero, one and two watch subscribers.

## Plan 07 changes

When this plan's step 4 is verified, plan 07 continues with these changes:

- The monitor uses `state.watch` per host instead of polling metrics every two
  seconds and catalogs every ten seconds. Its scheduling, backoff and fallback
  rules apply to connection setup and the polling fallback only.
- `host.metrics` becomes the `metrics` watch topic. A one-shot `host.metrics`
  request stays for older hosts and for `mesh ls`.
- `DashboardHostView` session and service lists are bounded to what the wall
  can show, plus visible and total counts. No unbounded slices cross into the TUI.
- Footprint budget on the Pi, measured with the Plan 284 heavy-job accounting
  or `/proc` sampling: dashboard resident memory under 40 MB, flat Go heap over a
  four-hour soak, a constant number of goroutines per host (the transport's
  read and keepalive loops plus one consumer) plus the UI, and redraws
  only on change or graph tick.

## Execution order

Each step is its own reviewed change with its gate measured on the Pi.

1. Change-driven reconcile and slow liveness persistence (§1).
2. Lean catalogs and the `mesh ls` sampling fix (§2).
3. `state.watch` publisher and client library, with bounded queues and resync (§3).
4. Connection and subscriber caps, lazy executable hashing, daemon accounting (§4).
5. Rebase `feat/dashboard` on `origin/main` and resume plan 07 on the watch.
