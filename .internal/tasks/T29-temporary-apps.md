# T29 — Private temporary apps

Status: Approved. Public sharing and automatic widget rendering are removed.

The app entry point copies source into an origin workspace, reserves a short
name and returns an HTTPS URL. Static sites and explicit HTTP server recipes
are private. The owner can inspect, update, renew, download or delete them.
Admitted app traffic extends a 24-hour deadline; expiry withdraws access before
origin cleanup. Used names remain reserved.

## Architecture

An independent private app registry owns allocation, browser grants, inactivity
deadlines and signed lifecycle acknowledgements. The origin owns source and
labelled workers. Replacing ordinary service snapshots cannot erase an app.
Terminal traffic remains direct between hosts.

- `internal/apps`: private registry, signed operations, origin staging and
  runtime recovery, admission, traffic accounting and trusted management pages.
- `internal/webauth`: pairing, owner grants, CSRF, revocation and app-scoped
  view tickets.
- `internal/daemon`: labelled-worker adapter, Unix-only owner requests,
  identity-pinned registry exchange and recurring reconciliation.
- `internal/edge`: ordinary service and tunnel routing. Public routing does
  not admit temporary apps.
- `internal/storage`: durable app state, replay acknowledgements and permanent
  hostname reservations shared with ordinary service and tunnel collision checks.
- `internal/apppill` and `web/app-pill`: preserved widget source, generated
  assets, transformer and interaction fixtures. Runtime app routing does not
  inject the widget or serve its routes.

Temporary-app content, management, pairing and upgrades require HTTPS and a
verified Tailnet connection before owner or browser checks. Browser credentials
stay on the management host. App code receives no owner key, management cookie
or CSRF token. Signed one-use origin admission prevents direct requests from
bypassing the private registry's checks.

Owner app operations enter the origin through its Unix socket or authenticated,
exact-host-key-pinned Mesh SSH. CLI commands take an owner host first and use
that host's configured registry. See [usage](../temporary-apps.md).

## Preserved widget build

Solid, Vite, TypeScript and Playwright remain development dependencies for the
reusable floating pill. React Grab adaptations retain their MIT provenance under
`third_party/react-grab-toolbar`. The Go transformer retains its bounded HTML
and encoding fixtures. These dependencies do not add controls to live apps.

Run `pnpm --dir web/app-pill install --frozen-lockfile` and
`pnpm --dir web/app-pill build` after changing that source. Commit generated
assets with their source changes. `scripts/sync-app-pill.mjs` regenerates the
pinned upstream adaptations. See [the widget guide](../../web/app-pill/README.md).

## Verification

The Go tests cover signing and replay, allocation, service/tunnel collisions,
staged updates and retry recovery, archives, expiry, leases, HTTP admission,
pairing, app-scoped views, CSRF and revocation. Private ingress checks must
reject internet requests and insecure requests before any pairing or content
operation. Removed sharing actions and fields must fail at their boundaries.

Origin routing uses immutable snapshots so readiness waits cannot block existing
requests. Fault-injection tests cover activation acknowledgements, receipt
persistence, interrupted uploads and updates, stream cancellation and cleanup.
Runtime process checks reject wildcard, LAN and Tailnet bindings and listener
ownership changes. Existing worker privileges still apply.

`integration/temporary_apps.sh` verifies CLI operations and rejected sharing.
`integration/temporary_app_server.sh` exercises labelled setup/server workers,
HTML/API/redirect/WebSocket forwarding, restart adoption and deletion. Live HTML
must contain no injected widget. Browser fixtures cover the preserved widget's
interaction behavior separately from runtime app routing.

Run the repository race, vet and integration gates. Verify ordinary named public
services and SSH tunnels alongside private apps, and confirm that an upgrade
preserves the owner's existing app source files.
