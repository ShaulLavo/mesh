# Temporary apps

Create a website from a local source directory on an enrolled Mesh origin:

```sh
mesh app create pc ./site
mesh app create pc ./backend --setup 'bun install' \
  --run 'bun run start --host 127.0.0.1' --port 3000
```

For a dependency-free server example with shared SQLite favorites, see
[A little color room](../examples/palette-server/README.md).

Use `local` for this machine. Remote operations use the origin's authenticated
Mesh SSH service, default port 2222. `--ssh-port` selects a different configured
port. The origin needs `--public-edge-target`; its identity must be in the edge's
origin allowlist. The edge reserves `apps.shaulavo.dev` for management. Existing
wildcard DNS, HTTPS, and trusted front-door forwarding must cover these names.

Creation returns a short URL such as `https://7k3d.shaulavo.dev`, initially private.
Source is copied; your input directory remains yours. Mesh excludes environment
files, credentials and generated dependency/cache directories, rejects links and
unsafe archive paths, and limits source to 64 MiB and 10,000 files. Dependency
installation requires an explicit setup command in the managed copy.

Server commands must bind only `127.0.0.1` or `::1`, on a free port from 1024 to
65535. Mesh supplies `PORT` and `HOST=127.0.0.1`, but your server must actually use
a loopback binding. Startup rejects wildcard, LAN and Tailnet listeners. This
feature uses the host's ordinary Mesh worker privileges. Ordinary `serve` routes
cannot alias an app's port or managed directories, even before a demand server
has started. Choose a separate app port.

On Linux, payloads default to `/work/mesh/apps`. Set daemon `--app-data-root` for a
different absolute workload directory. Mesh checks data SSD mounts and requires
at least 128 MiB available before upload or setup. App caches and local data live
with its managed copy and are deleted with it.

## View and share

For Tailnet hosting, configure [automatic owner access](../examples/tailnet-gateway/README.md).
Mesh recognizes your devices by matching their Tailscale account to the app's
configured origin. Fresh browsers can view private apps and get the Make public
or Make private button without pairing. Public visitors get no owner authority.
This check runs on requests to both private and public apps.

For deployments without automatic Tailnet access, private apps send you to `https://apps.shaulavo.dev`, where the
browser displays a one-use approval code. Approve on the owner host:

```sh
mesh app browser approve pc CODE
```

Keep the pairing page open. It checks for approval automatically and continues
as soon as the owner approves. Checks preserve the code until its ten-minute
expiry. Pairing grants that browser management
rights for apps owned by `pc`; it grants no rights for another origin's apps.
The edge issues a separate view-only cookie for the private app. Public visitors
need no pairing and cannot change ownership or visibility.

Click the small edge tab to expand the React Grab pill. Drag or flick it to an edge;
its position survives reloads. The link button copies the app URL and briefly shows
a checkmark. Clipboard access requires HTTPS. The lock button opens
a trusted visibility confirmation for recognized owners, or browser pairing for
visitors. It appears only after the management origin responds. The chevron collapses
the pill. Owner authorization runs in a hidden frame; there is no controls menu.
Management pages remain separate from app content. Renew, delete and source
download are also available through the CLI.

```sh
mesh app public pc 7k3d
mesh app private pc 7k3d
mesh app browser list pc --json
mesh app browser revoke pc BROWSER_ID
```

Making private blocks new non-owner requests and closes their active streams.
Revocation invalidates browser grants and view cookies. Downloading source always
requires owner authorization, including when the website is public.

## Manage the lifecycle

```sh
mesh app list pc --json
mesh app inspect pc 7k3d --json
mesh app update pc 7k3d ./site
mesh app renew pc 7k3d
mesh app download pc 7k3d ./saved-app.tar.gz
mesh app delete pc 7k3d
```

Updates preserve the URL and visibility and renew the deadline. Uploads are staged
and checked before replacing working source. Startup failures return an explicit
error; updates do not promise uninterrupted service. Server updates require their
explicit `--run`, `--port`, and optional `--setup` recipe again. Downloads write a
new archive atomically and refuse to overwrite an existing destination.

App HTTP traffic, streamed data, and WebSocket application frames extend its
24-hour deadline. Bots count. Pill assets/status, denied requests, idle upgrades,
and WebSocket ping/pong do not. Visibility changes do not renew it. `renew` does.

Expiry or deletion first removes access, then stops workers and deletes managed
source, dependencies, data and retained app output. The original source and
external databases remain yours. If the origin is offline, cleanup stays pending;
its short edge lease stops serving/running during a partition. Cleanup completes
when the origin reconnects. If the edge stops listing an app, for example after
losing its state, the origin stops the app when its lease lapses. It deletes the
managed copy 24 hours later unless the edge lists the app again first. Expired
names are never reassigned or revived.

HTML injection supports UTF-8 and common single-byte encodings, gzip/Brotli,
header/meta CSP, and strict nonce policies. Binary/API responses remain intact.
Unsupported HTML encodings and oversized tokens fail explicitly. Browser checks
cover Chromium and WebKit; Safari device and production deployment checks are
still pending rollout.
