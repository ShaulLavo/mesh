# Mesh app pill

The Solid control mounts in an app's Shadow DOM. It is a floating dot; tapping the
dot grows a pill of actions out of it, and tapping the dot again folds them away.
`src/dock.ts` owns placement. The open pill is a row of 44px touch targets, the
dot's and one per action. One rule puts the dot's center 38px from the safe edge
(a 16px gap plus half the pill) and the open pill 16px from it, with the dot in
the pill's end cap and the actions toward the middle of the edge. Every path uses it: load, drag, flick,
viewport change, keyboard and toggle. `src/drag.ts` adapts React Grab's drag
gesture. The
browser build embeds `pill.js` and `pill.css` in the Go binary; installed Mesh
hosts need neither Node nor Playwright.

Use Node 24 or newer and the pinned pnpm version from `packageManager`:

```sh
cd web/app-pill
pnpm install --frozen-lockfile --store-dir /work/cache/pnpm
pnpm build
```

`build` checks TypeScript and bundles the assets into `internal/apppill/assets`. Commit generated assets
alongside their source changes.

The bundle retains the MIT notices for Solid and React Grab from `third_party`.

## Browser verification

Playwright is pinned as a development dependency for repeatable interaction
checks. The harness starts an ephemeral loopback HTTP server, serves the actual
compiled assets under a strict CSP, and tests Chromium and WebKit with normal
and reduced motion. It checks the collapsed dot, expansion, mouse dragging,
synthetic touch dragging, keyboard docking, stored position, management-frame
status messages, and rejection of messages from an incorrect origin or window.

The frame is a labelled mock. Browser approval, private view tickets, ownership,
CSRF, signed origin forwarding, and lifecycle recovery have separate Go tests:

```sh
go test -race ./internal/apps ./internal/apppill ./internal/webauth
```

Use previously installed browser binaries on this Linux host:

```sh
PLAYWRIGHT_BROWSERS_PATH=/work/cache/ms-playwright \
MESH_CHROMIUM_EXECUTABLE=/usr/bin/chromium \
pnpm --dir web/app-pill test:browser
```

`tests/snap.mjs` docks the dot and the pill on all four edges of iPhone portrait
and landscape viewports by drag, flick, keyboard, resize and reload, and requires one
rest position per edge and state. The placement math has fast unit tests:

```sh
pnpm --dir web/app-pill test
```

Run the browser command from the repository root. `test:browser` builds first and fails
if either browser is unavailable or a check fails. `MESH_CHROMIUM_EXECUTABLE` is
optional; without it Playwright uses its bundled Chromium. Neither dependency
installation nor the test downloads browsers. On a fresh machine, install them
explicitly after choosing a suitable cache directory and checking its mount and
free space:

```sh
PLAYWRIGHT_BROWSERS_PATH=/work/cache/ms-playwright \
pnpm --dir web/app-pill exec playwright install chromium webkit
```

On another platform, replace the cache paths with a configured data directory.
Headless WebKit coverage does not replace Safari verification on a Mac or phone.

Screenshots are disabled by default. To save expanded and collapsed screenshots
for both motion settings, set `MESH_PILL_ARTIFACTS_DIR` to a disposable directory,
for example `/work/tmp/mesh-pill-browser` on Linux or a directory under the
system temporary directory on macOS.
