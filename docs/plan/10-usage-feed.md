# Generic usage feed on the TV dashboard

Status: Approved, 2026-10-02. Planning/design only; no runtime changes in this PR.
The owner's approved October 2 normal and no-data designs are authoritative;
implementation follows independent review and merge of this planning PR.

Canonical source inventory, contract and gateway implementation plan:
[Fregat Plan 289](https://github.com/ShaulLavo/fregat/blob/main/plans/289-proxy-usage-feed.md).
This extends [07: terminal fleet dashboard](07-dashboard.md); it does not replace
its host watch transport, session details, theme or Pi setup work.

## Boundary and configured accounts

Mesh is a generic read-only viewer of a versioned JSON snapshot. The producer
belongs to Fregat's existing Claude/GPT gateway tooling beside CLIProxyAPI.
It captures Claude quota headers from normal gateway traffic and reads the
proxy's already-recorded per-account GPT quota/cooldown state through localhost
management. Fregat's present native usage cache is outside this build; later
Fregat consumption of this feed is a separate follow-up in Plan 289.

The approved configured identities and order are:

| Provider | Short account label | Plan | Observation path |
| --- | --- | --- | --- |
| Claude | `shaul9191` | Max | Passive quota headers on the owner's own Claude Code traffic through our gateway |
| Codex | `shaul9191` | Pro | CLIProxyAPI's per-credential cached observations |
| Codex | `shaul.lavochkin` | Pro | CLIProxyAPI's per-credential cached observations |

**Both Codex accounts are Pro. Claude is gateway-only and is never pooled or
proxied through CLIProxyAPI.** No Claude session traffic means its last actual
observation ages into stale data; there is no background Claude quota probe.
Providers own ordered lists of accounts: Gemini or another provider is an
additional group, with no provider-specific layout branch.

No provider collector, OAuth handling, quota probing, account selection or reset
logic belongs in Mesh. No new dependency is needed: Go's HTTP/JSON support and
existing dashboard projection/rendering suffice. Keep the data out of fleet host
metrics and the session protocol.

## Producer prerequisite and URL

Management is already enabled on loopback port **18317**. Its private key file is
`/work/cli-proxy-api/management-key`. The Mesh `/ai` frontend is on **8318**;
installed runtime configuration reports the gateway backend on **18318** and
CLIProxyAPI on **18317**. These operational ports are not portable test constants.
The producer uses validated runtime configuration. Management stays loopback-only;
the key's contents never enter a feed, log, screenshot or Pi configuration.
There is no management enablement or proxy restart prerequisite left in this
plan, and this documentation-only PR makes no live changes or provider calls.

A dedicated sanitized directory is served through a **tailnet-only isolated
static route**, for example `https://omarchy.mesh.shaulavo.dev/ai-usage/v1.json`.
This example route is not provisioned by this PR. It is separate from the
on-demand `/ai` backend and keeps serving the last file while inference sleeps.
Static serving belongs to the existing Mesh daemon, not a new collector service.

Feed reads must not start `/ai`, extend its idle deadline, invoke readiness or
model discovery, touch providers, refresh credentials, drain the proxy usage
queue, or wake a host. An unreachable producer means cached/stale data on the Pi.
Tailnet ACLs and route isolation protect the sanitized feed; ordinary browser
Origin/device pairing from Fregat is not the Go client's authentication mechanism.

## Contract to consume

Use Plan 289's JSON v1 schema, not a provider response shape:

- `schemaVersion`, publication `generatedAt`, and all known `accounts`.
- Account opaque stable `id`, generic `provider`, safe `label`, `plan`, `state`,
  `source`, cache-inspection `checkedAt`, actual quota-observation `lastSeenAt`.
- `routing {mode, active, lastServedAt}`: active is boolean or **null**;
  cooldown contains a sanitized reason/recovery time or null.
- `windows[]`: stable ID, label, `usedPercent`, `resetsAt`, `windowMinutes`,
  normalized status, `lastSeenAt`, source. Missing percentage/reset/length is null.

Publication and cache inspection do not refresh observation age. Keep windows
independent; never sum percentages, derive quotas from token counters, assume a
primary window is five hours, or infer 100% used from a generic 429. Unknown
account selection stays unknown. Installed proxy management's `active` is
credential availability and its recent-request counts are ten-minute buckets;
neither proves which account is currently serving. Multiple accounts can serve
concurrently. `last served` is permitted only with positively attributed traffic;
it identifies the last account known to have served a response, not an account
currently in flight. `rotating` describes pool membership; `cooldown` describes
an observed account-routing fact independently of a window's percentage.

Preserve all known identities at startup with no-data/null timestamps. Preserve
last valid data through failures. After a reset passes, show `reset passed ·
awaiting traffic` or no current data; never refill to 0% without an observation.

## Implementation

- [ ] Add optional sanitized-feed URL to existing local config and its validation.
      With no URL, keep today's dashboard unchanged. No provider secrets/config.
- [ ] Add a small client/projection beside `internal/cli/dashboard_monitor.go`
      and dashboard types. At most one HTTP request per 60 seconds, one in
      flight, bounded timeout and 64 KiB response, normal TLS verification.
      Cancel with monitor shutdown. No wake, activation or provider redirects.
- [ ] Strictly validate v1 values. Unsupported schema, malformed/oversized data,
      timeout or HTTP failure retains the previous valid snapshot whole and
      exposes freshness/unreachable state; do not replace it with an empty view.
- [ ] Compute reset countdown locally. Use each window's lastSeenAt for age and
      a 15-minute stale threshold. Keep provider/account/window order stable.
      Feed fetches never change lastSeenAt or fabricate routing information.
- [ ] Compose the panel with existing `internal/tui/dashboard_tables.go` and
      `dashboard_render.go` glyphs/themes. Keep renderer provider-agnostic.

## Authoritative 160×45 wall layout

![Approved OLED layout: four hosts and three accounts, illustrative usage](images/usage-panel.png)

![Approved OLED no-data layout: all three configured accounts remain visible](images/usage-panel-no-data.png)

The images are exact copies of the owner's approved October 2 normal and no-data
PNGs. They supersede the earlier three-row account and overflow mockups. Both
are 1920×1080 renderings of a 160×45 terminal grid, with OLED high contrast as the
TV default. The header's `MOCKUP` label and all AI values are illustrative, not a
live Pi capture.

Rows 0–26 retain four host cards and the host count, ordered by installed RAM:
`pc`, `macbook-air`, `vps`, `pi`. All four rows of each CPU/RAM graph remain.
Rows 27–43 retain the three-column split:

| Columns | Width | Content |
| --- | ---: | --- |
| 0–56 | 57 | Sessions: both live sessions, HOST / ID / NAME / STATE / COMMAND (launch), visible/total counts |
| 57 | 1 | Gap |
| 58–104 | 47 | Services: all seven services in live order, HOST / SERVICE / STATE / AGE, ready/failed/idle totals and 7/7 visible |
| 105 | 1 | Gap |
| 106–159 | 54 | AI plans: 17 rows including frame, 50 inner cells, three accounts and six full 28-cell meters |

Each account takes **five rows**: one identity/age line, then two lines per
window. Each provider has exactly one heading, sharing its first account's
identity line; following accounts are indented beneath it. The identity line
shows the short account name, plan, observed routing badge where available, and
`seen` / `stale` age. The two visible windows per account are **5h** and **Weekly**:

1. Window label, `% used`, `% left`, and `resets <countdown>`.
2. The full 28-cell used-share meter, status dot and `OK` / `high` / `exhausted`.

The normal sample shows Claude Max `shaul9191` at 20% / 66%, Codex Pro
`shaul9191` at 42% / 54% with `last served`, and Codex Pro `shaul.lavochkin`
at 100% / 97% with `cooldown` and `stale 18m`. These are sample observations,
not claims about today's provider quota or proven live account selection.

The no-data sample retains the same provider groups, labels, plans and five-row
slots, with `seen —`, `No data yet`, and `Waiting for normal traffic` in each
window slot. No unobserved routing badge or filled meter appears. Expected-window
placeholders describe the display slots, not an unobserved real allowance.

Sessions gives 38 columns and Services gives 17 columns to AI plans compared
with the historical v2 design. Both live sessions and all seven services still
fit. Keep both host names in full; session names and launch commands shorten
first. Per-session age stays on the host card. Attention retains rows 39–42 and
two body lines for an unhealthy service's full name and reason when needed; it
is absent while there are no failures. Service row 43 is spare. No host
measurement, graph height, live session, service or reported failure is sacrificed.

Keep model-scoped windows in the generic feed, with an extra-window count on
the account line when they do not fit. Show explicit omitted-account counts
when capacity is exceeded. Keep provider/account/window order stable; never
auto-page or scroll. At 80×24 and intermediate widths, use compact account/window
text, shorten session detail first and preserve fleet/failure visibility. Below
the supported minimum, use the dashboard's resize state. Test these separately;
the approved PNGs specify the 160×45 wall, not unverified smaller layouts.

## Meter, theme and freshness rules

Each window is a ratio against its own allowance. Use the existing 28-cell `▪`
meter; percentages are never summed into a pool.

- OLED: green below 75%, amber at ≥75%, red when exhausted. Use each theme's
  existing `good`, `cached` and `failure` semantic roles, not account colors.
  The status dot and `OK`, `high`, `exhausted` words duplicate color encoding.
- Numbers and status words use the existing text role; only meter marks and
  status dots use status colors. Tracks and graph fills stay recessive.
- The pale `│` is elapsed share: `100 × (1 − (reset − now)/windowLength)`,
  clamped to 0–100. It is an even-use guide, not predicted request capacity.
  Hide it for stale, exhausted, reset-passed or unknown-length observations.
- `seen 2m` ages the actual upstream observation. `stale 18m` preserves the
  last-seen values after 15 minutes. File publication never refreshes this age.
- A cooldown is a routing fact. A generic HTTP 429 never establishes 100% used;
  the exhausted sample represents an illustrative provider quota observation.
- When reset passes, retain explicitly historical data (`reset passed · awaiting
  traffic`) or no current data. Never silently refill the meter.

Support **all six existing themes** unchanged: `current`, `rose-pine`,
`rose-pine-moon`, `oled`, `kanagawa`, `gruvbox-material`. OLED is the approved
TV default; selecting another theme changes semantic role values, not layout,
identity, thresholds or wording. The approved OLED text/status roles all clear
4.5:1 against black, checked in the design generator against Mesh's theme table.
Do not invent colors or claim these two PNGs verify the other five themes.
Implementation must check every theme plus Unicode/ASCII fallbacks. Full account
names, plans, routing badges, ages and window values must fit; only launch
commands and session names may shorten first.

The approved source report's TXT grids are the equivalent readable tables;
its generator asserts exactly 45 rows of 160 Unicode cells, validates panel
budgets, and preserves all four host cards between usage states. A terminal TV
has no hover interaction. No chart generator, browser preview, new palette or
additional mockup variants are introduced by this documentation PR.

## Narrow checks and delivery

- [ ] `httptest` fixtures, injected clock/client: v1/no-data/two rotating accounts,
      per-window observation ages, cancellation, at-least-60-second cadence,
      bounded bodies/timeouts, bad schema and last-good retention. Counters prove
      no extra request caused by screen renders or multiple consumers.
- [ ] Dashboard projection/render snapshots at 160×45 with four hosts/three
      accounts, provider headings once and five rows per account; all six themes;
      no-data/stale/75%/100%/reset-passed, unknown routing, model windows,
      account overflow, ASCII fallback, 80×24 and resizing. Run only affected Go
      package/tests; `go vet` and the existing delivery gates when code lands.
- [ ] Integration proof with inference asleep: repeated static GETs leave `/ai`
      asleep and its idle deadline unchanged. With producer unreachable, keep
      prior data and observe no wake call. Production/provider probes are not CI.
- [ ] Read real terminal screenshots against both approved mockups, then verify
      on the Pi TV. Keep machine-specific proof outside portable committed tests.
- [ ] Commit/push owned paths and obtain independent review before implementation.
      Ship the implemented gateway/static route and Mesh/Pi only in the subsequent
      implementation run. This planning PR makes no live changes, restarts,
      provider calls or deployments and is not merged by its author.

## Acceptance

All three configured accounts are independently visible, including no-data/stale
ones, with both Codex labels shown as Pro and Claude `shaul9191` as Max through
the gateway only. Every observed window has used/left/reset/freshness; unknown
stays unknown. Reads add **zero provider calls, zero inference demand, and zero
host wake-ups**. Four hosts, both live sessions, all seven services and any
failure reason remain visible at the target TV size. Setup does not give the Pi
provider credentials. Exact active attribution can remain unknown without
blocking this feature. Fregat's own active probes remain unchanged until its
separately scoped feed-consumer follow-up.
