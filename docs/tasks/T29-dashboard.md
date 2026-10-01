# T29: passive fleet dashboard

Status: Approved, implementation in progress.

Implement [the dashboard plan](../plan/07-dashboard.md) as a passive fullscreen
view with no navigation. Use the owner's active Omarchy Last Horizon palette.
The implementation worktree is `/work/worktrees/mesh/dashboard`, branch
`feat/dashboard`. Preserve the unrelated audit checkout.

`github.com/shirou/gopsutil/v4` is justified for no-cgo Linux/Darwin CPU counters,
memory and uptime. Pin it in go.mod; use demand-driven shared sampling, never
the blocking CPU percentage helper or an always-running sampler.

## Ownership and contracts

- Metrics lane owns `internal/hostmetrics`, `internal/protocol/metrics.go`,
  changes to protocol.Control, daemon metric wiring, go.mod/go.sum and focused tests.
- Rendering lane owns `internal/tui/dashboard*.go` and focused tests only.
- Root owns CLI types/monitor, Cobra and bootstrap wiring, integration, palette
  mockup changes, verification, and Pi setup.

UI domain types are in `internal/cli/dashboard_types.go`. Emit immutable retained
views through DashboardWatch. State strings are available/unavailable/unsupported.
MeasuredAt uses local monotonic time; zero means never measured.

Metrics protocol contract for the lanes:

```go
const TypeHostMetrics = "host.metrics"
const TypeHostMetricsResult = "host.metrics.result"
type MetricValue[T any] struct {
    State string `json:"state"`
    Value T `json:"value"`
    Sample string `json:"sample"`
    AgeMillis int64 `json:"ageMillis"`
    Problem string `json:"problem,omitempty"`
}
type MemoryUsage struct { TotalBytes, AvailableBytes uint64 }
type Temperature struct { Sensor string; Celsius float64 }
type HostMetrics struct {
    CPU MetricValue[float64]
    Memory MetricValue[MemoryUsage]
    Temperature MetricValue[Temperature]
    Uptime MetricValue[uint64] // seconds
}
// Control gains Metrics *HostMetrics `json:"metrics,omitempty"`.
// ValidateHostMetrics(HostMetrics) error checks wire data at the client boundary.
```

Collectors hide platform APIs. Each request has independent metric sample IDs
and ages; retain last successful fields on collection failure. One demand sampler
per daemon, coalesced across viewers; reset CPU baseline after >10-second gap or
counter reset. Reject nonfinite values, impossible memory ratios, invalid ages
and missing sample identities for available wire readings. Existing host.info
identity verification must precede metrics and all catalog reads on a one-shot
connection. No Wake, DialHost recovery, or session attachment in any dashboard path.

Last Horizon palette read from the PC's active theme on 2026-10-01:

```
background #0c0b0c   surface #090809   border #584e51
foreground #FAFCFB   secondary #cfd3cd
CPU/accent #b59790   RAM #a5a0b6   reachable #87a9b0
error #c38b7b        stale #c4d8e2
```

Acceptance: real data, passive 4K view, 80x24 fallback, two-minute graphs with
gaps and fixed percent scales, stale/unsupported/refused distinctions, no auto
wakes, stable identities, bounded fair reads, signal cleanup, older-daemon
catalog compatibility, ARM64/Darwin builds. Tests cover the actual no-wake wiring
and meaningful boundary/cancellation cases. Run the repo checks once integrated.
