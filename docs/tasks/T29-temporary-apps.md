# T29 — Temporary apps

Status: Approved; implemented for PR review. Production rollout is pending merge.

The app entry point copies source to an origin workspace, keeps an independent
edge reservation, and returns a four-character `https://7k3d.shaulavo.dev` URL.
Static sites and explicit HTTP server recipes start private. The owner can
publish, privatize, renew, download, update, or delete them. Every admitted viewer
gets the Solid floating pill. App requests and data extend a 24-hour deadline;
expiry denies access before origin cleanup. Used names remain reserved.

## Architecture

Use an independent app registry rather than putting app routes in ordinary
`serve` snapshots. Snapshot replacement must never erase an app or its access
policy. The origin owns source and labelled workers; the edge owns the global
name, browser grants, visibility and deadline. Ordinary terminal traffic remains
direct. This was selected after comparing both designs against actual snapshot,
worker, storage and front-door implementations.

- `internal/apps`: signed domain operations, origin staging/runtime/cleanup,
  app HTTP admission, traffic accounting and trusted management pages.
- `internal/webauth`: durable pairing, owner grants, CSRF, revocation and
  single-use tickets for app-scoped private viewing.
- `internal/apppill` and `web/app-pill`: embedded Solid shell and streaming HTML
  transformation. Actual React Grab drag/position helpers are copied with MIT
  provenance under `third_party/react-grab-toolbar`.
- `internal/daemon`: labelled-worker adapter, Unix-only owner requests,
  pinned edge exchanges and recurring reconciliation.
- `internal/edge`: host dispatch after front-door validation and before
  snapshots/tunnels, with shared client/origin/global admission budgets.
- Migration 9: opaque role/auth state and permanent app-name reservations.
  Name allocation and app state/replay acknowledgement commit atomically with
  the existing tunnel/service collision checks. State compatibility advances to 9.

Browser credentials stay on `apps.shaulavo.dev`. Embedded controls open a
separate trusted confirmation page for mutations. Public app code receives no
owner key, management cookie or CSRF token. Signed admission is also required
at the origin, preventing direct Tailnet requests from bypassing the edge gate.
Owner app operations enter the origin through its Unix socket or authorized,
exact-host-key-pinned Mesh SSH; an unsigned Tailnet RPC cannot ask it to sign.

CLI commands consistently take an owner host first and use that host's configured
`--public-edge-target`. There is no second per-command edge address book. See
[usage](../temporary-apps.md).

## Dependencies and build

- `github.com/andybalholm/brotli` decodes Brotli HTML before streaming injection.
- `golang.org/x/net/html` supplies a bounded HTML tokenizer. Its existing pinned
  version becomes a direct dependency; other Go dependencies retain their pins.
- Solid JS is required to adapt React Grab's Solid helpers. Vite and TypeScript
  build and typecheck the pill. Playwright is a development dependency for the
  reproducible Chromium/WebKit interaction harness. Versions and pnpm lockfile
  are pinned; CI rebuilds and checks embedded assets for drift.

Run `pnpm --dir web/app-pill install --frozen-lockfile` and
`pnpm --dir web/app-pill build` after changing the pill. Generated JS/CSS are
embedded and committed, so ordinary Mesh builds and release archives need only
Go. `scripts/sync-app-pill.mjs` deterministically regenerates the upstream copies
and explicit adaptation from the pinned source; it never silently updates upstream.

## Verification

The Go race suite and vet cover request signing/replay, allocation transactions,
service/tunnel collisions, staged updates and retry recovery, source boundaries,
exact expiry, partition leases, actual HTTP edge-to-origin forwarding, pairing,
private tickets, CSRF, revocation, and public-to-private admission races.

Origin routing uses an immutable snapshot so setup and readiness waits cannot
block existing app HTTP requests. Fault-injection regressions cover lost
activation acknowledgements with direct retries and restart reconciliation,
receipt persistence failures, upload consumption, and update revision recovery
with expiry renewal, stream cancellation, and previous-workspace cleanup.

`integration/temporary_apps.sh` verifies the real CLI and disabled-feature boundary.
`integration/temporary_app_server.sh` runs actual labelled setup/server workers,
HTML/API/redirect/WebSocket forwarding, daemon restart adoption and deletion.
Ordinary serving rejects app-owned ports and managed directory aliases in both
registration directions, including dormant service recipes.
The existing integration suite covers worker survival, kill escalation, TLS,
front-door trust, SSH, packaging, and ordinary serving. Browser interaction checks
passed in Chromium and WebKit for strict CSP, touch targets, dragging/docking,
keyboard movement, reload position, and reduced motion. A real Safari device and
production wildcard front door still require verification after deployment.

Runtime startup checks require a free port and exact loopback bindings. App
scripts retain the existing Mesh host privileges; this is a disposable hosting
feature, not OS process/network isolation. Linux payloads default to
`/work/mesh/apps`; configured data SSD mounts and free space are checked.
