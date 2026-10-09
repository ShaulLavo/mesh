# Temporary apps

Temporary apps are private websites with a 24-hour inactivity deadline. Mesh
serves their content without adding a widget. App content, management pages,
pairing and WebSocket upgrades require HTTPS and a verified Tailnet connection.
Browser grants do not make an app reachable from the internet.

The examples use `new.example` from the configured
[deployment domains](deployment-domains.md). Replace it with your domain.

## Create and update

Create a website from source on an enrolled Mesh origin:

```sh
mesh app create pc ./site
mesh app create local screenshot.png clip.webm
mesh app create pc mock.html style.css image.png
mesh app create pc ./backend --setup 'bun install' \
  --run 'bun run start --host 127.0.0.1' --port 3000
```

Supply a directory to copy its contents, or select loose files to make a page.
Images and videos appear in a gallery; other files get download links. A single
HTML file becomes the app's index page. An HTML page supplied with its assets
keeps their relative filenames. Use a complete website directory for nested
assets. Several HTML directories can also appear in a gallery.

Use `mesh app update HOST ID SOURCE...` for revisions at the same URL. Both
commands support `--json`. Server recipes with `--run` or `--setup` require a
single source directory. Loose-file preparation uses the system temporary
directory, configurable with `TMPDIR`, and removes staging files after upload
or failure.

Use `local` for this machine. Remote operations use the origin's authenticated
Mesh SSH service, default port 2222. `--ssh-port` selects a different configured
port. The owner host sends signed lifecycle operations to its configured app
registry. Configure its daemon with `--app-registry-target /path/to/registry.json`.
The file pins the registry identity, Tailnet name, control port, and WebSocket
path. Private app maintenance runs independently of ordinary public services;
`--public-edge-target` configures only explicitly published services.

App and management names must resolve to the private Tailnet ingress,
with HTTPS certificates covering them. See the
[Tailnet gateway example](../examples/tailnet-gateway/README.md).

Creation returns a short URL such as `https://7k3d.new.example`. Source is
copied; your input directory remains yours. Mesh excludes environment files,
credentials and generated dependency/cache directories, rejects links and
unsafe archive paths, and limits source to 64 MiB and 10,000 files. Dependency
installation requires an explicit setup command in the managed copy.

Server commands must bind only `127.0.0.1` or `::1`, on a free port from 1024 to
65535. Mesh supplies `PORT` and `HOST=127.0.0.1`, but the server must use a
loopback binding. Startup rejects wildcard, LAN and Tailnet listeners. Ordinary
`serve` routes cannot alias an app's port or managed directories, even before a
demand server has started. Choose a separate app port.

App commands run with the host's ordinary Mesh worker privileges. On Linux,
payloads default to `/work/mesh/apps`. Set daemon `--app-data-root` for a different
absolute workload directory. Mesh checks data SSD mounts and requires at least
128 MiB available before upload or setup. App caches and local data live with
the managed copy and are deleted with it.

For a dependency-free server example with shared SQLite favorites, see
[A little color room](../examples/palette-server/README.md).

## Open a private app

Connect your device to the Tailnet, then open the returned HTTPS URL. Mesh checks
the connection's verified client address before it evaluates owner or browser
authority. Caller-supplied HTTP forwarding headers cannot identify a device.
Apps reject requests that do not meet this private ingress policy, including
requests carrying a previously approved browser cookie.

Automatic owner access matches the device's Tailscale account to the app's
configured origin. Where browser approval is required, open
`https://apps.new.example` from a permitted Tailnet device. Click **Start pairing**
to get a one-use code. Opening or refreshing the page does not allocate a code.
Approve it on the owner host:

```sh
mesh app browser approve pc CODE
```

The terminal shows the browser's User-Agent summary, source IP and pending age,
then asks `Approve this browser? [y/N]`. Approve only a code requested in your own
browser. These details are unverified hints, not proof of identity. Empty or
negative answers cancel. Scripts can pass `--yes` to skip the human check.
Agents must not approve a code the owner did not ask them to approve.

Keep the pairing page open. It checks for approval and continues automatically.
Refreshes reuse the pending code until its ten-minute expiry or eviction.
Pending codes disappear on a registry restart; approved grants remain durable.
Pairing grants management rights for apps owned by that origin. An app view
uses a separate, app-scoped cookie. Management credentials stay on the management
host and are stripped before requests reach an app server.

Pairing has per-address and aggregate limits. Shared NATs and IPv6 delegations
share those limits. Pending capacity replaces an old unapproved code; approved
records are never evicted. If a code expires or is evicted, start again from the
pairing page.

```sh
mesh app browser list pc --json
mesh app browser revoke pc BROWSER_ID
```

Revocation invalidates browser grants and view cookies. App requests enforce
same-origin admission and framing policy. WebSocket upgrades require the app's
exact Origin. Apps must keep GET requests read-only and protect mutations from
legacy clients that omit browser headers.

## Manage the lifecycle

```sh
mesh app list pc --json
mesh app inspect pc 7k3d --json
mesh app update pc 7k3d ./site
mesh app renew pc 7k3d
mesh app download pc 7k3d ./saved-app.tar.gz
mesh app delete pc 7k3d
```

Updates preserve the URL and renew the deadline. Uploads are staged and checked
before replacing working source. Startup failures return an explicit error;
updates do not promise uninterrupted service. Server updates require their
`--run`, `--port` and optional `--setup` recipe again. Downloads write a new
archive atomically and refuse to overwrite an existing destination.

Admitted app HTTP traffic, streamed data and WebSocket application frames extend
the 24-hour deadline. Denied requests, management polling, idle upgrades and
WebSocket ping/pong do not. `renew` explicitly extends the deadline.

Expiry or deletion first removes access, then stops workers and deletes managed
source, dependencies, local data and retained app output. Original source and
external databases remain yours. If the origin is offline, cleanup stays pending;
its short registry lease stops serving or running during a partition. Cleanup
completes when the origin reconnects. If the registry loses an app record, the
origin stops it after its lease lapses and retains the managed copy for another
24 hours before cleanup. Expired names are never reassigned or revived.

## Preserved widget source

The floating pill's source, generated assets, Go transformer and interaction
fixtures remain in `web/app-pill` and `internal/apppill` for reuse in another
project. Temporary apps do not inject it, serve its assets or expose widget
management routes. See the [widget source guide](../web/app-pill/README.md).

Public sharing commands and APIs have been removed. Existing app source files
are preserved when upgrading; former sharing metadata cannot enable internet
access. Ordinary named public services and SSH tunnels keep their own behavior.
