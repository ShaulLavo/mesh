# T11 — Serving core

**Status:** complete · **Blocked by:** nothing (T04 landed) · **Owns:** `internal/serve/`

## Goal

An origin daemon can serve three things over HTTP, on the tailnet, with no public
exposure and no VPS involved. This is the whole feature minus the internet.

## Service types

- **static** — serve a directory. Resolve symlinks and refuse to escape the root.
- **files** — a browsable listing with download links. Generated HTML, so it must
  emit correct links under whatever path prefix it is mounted at.
- **proxy** — reverse-proxy to `127.0.0.1:<port>` on the origin machine. Forward
  `X-Forwarded-*`, handle WebSocket upgrades, and stream rather than buffer.

```go
package serve

type Kind string // "static", "files", "proxy"

type Service struct {
    Name          string // route name, e.g. "blog"
    Kind          Kind
    Target        string // directory path, or port for proxy
    PublicName    string // empty for tailnet-only; otherwise the exact public hostname
    WakeOnRequest bool
}

// Handler returns the http.Handler for s, mounted at prefix.
func Handler(s Service, prefix string) (http.Handler, error)
```

## Where services live

Persist them in the origin's SQLite (T03 owns the store; add a `services` table
there rather than a second database). Services survive a daemon restart and come
back automatically. A service whose directory has vanished registers but reports
unhealthy rather than crashing the daemon.

The daemon serves them on its existing HTTP listener (the one T05 builds for the
WebSocket) under a reserved prefix, so serving costs no extra port.

## Build the root resolver so someone else can call it

T16 serves these same roots over SFTP (D19), and it must not write a second path
resolver. Export the "resolve this request path inside this root, or fail"
function rather than burying it in an `http.Handler` closure. A filesystem API
leaks a whole tree where an HTTP handler leaks one file, so the shared one is the
one that gets the attention.

## The part that needs care

Path traversal on `static` and `files`. A request for `/files/../../etc/passwd`,
in every encoding a client can express it, must not escape the served root. Test
this deliberately, including URL-encoded and double-encoded traversal, symlinks
pointing outside the root, and null bytes.

Since the tailnet-only default means these are reachable by anything on the
tailnet, "it is private anyway" is not an argument for being sloppy here.

## Acceptance

- Go tests per service type, including a table of traversal attempts that must
  all return 404 or 400 and never a file outside the root.
- A `files` listing mounted at `/files` emits links that work, and the same
  handler mounted at `/` also works. Prefix handling is the bug everyone ships.
- Proxy passes a WebSocket upgrade through intact.
- Serving survives a daemon restart with services intact.

## Out of scope

TLS, public exposure, the edge, and the CLI. This task ends at "the origin daemon
answers HTTP on the tailnet".

## Implementation

`internal/serve.ResolveRoot` is the shared canonical-path parser for previews
and Realpath-style results. `OpenRootEntry` is the shared HTTP and SFTP access
boundary. They bound repeated URL decoding, reject null bytes and parent
traversal, resolve symlinks, and verify the canonical result remains below the
canonical root. A missing root returns `ErrRootUnavailable` without preventing
registration or daemon startup.

Canonical resolution remains separate from file access because previews expose
the canonical target and Mesh supports absolute symlinks whose targets stay
inside the root. Go's `os.Root` deliberately rejects every absolute symlink.
Static and files handlers therefore resolve first, convert the canonical target
back to a confined relative path, and open it through an anchored `os.Root`.
They inspect, list, and serve from that same descriptor. A symlink or directory
swap between resolution and open can make the request fail, but cannot redirect
the open outside the root. Nonblocking opens also prevent a regular-file-to-FIFO
swap from hanging a handler. Public-directory scans use the same rooted open
boundary while retaining canonical paths for cycle and alias detection.

`FuzzResolveRootConfinement` exercises valid, malformed, repeatedly encoded,
and symlinked paths against both a real root and a symlink alias, then checks
the rooted descriptor against the resolved entry. A focused boundary test
exposed and closed the previous off-by-one rejection at exactly eight decoding
layers.

`serve.Handler` implements static sites, generated file listings, and loopback
reverse proxies. File links retain their mount prefix at both `/` and nested
routes. The reverse proxy strips the service prefix, sets `X-Forwarded-*`, flushes
streamed responses, and passes WebSocket upgrades.

`serve.Registry` publishes complete route snapshots atomically and dispatches by
longest prefix. It rejects any route that overlaps the configured daemon
`WebSocketPath`, including an ancestor or descendant. `service.upsert`,
`service.list`, and `service.delete` are additive daemon controls for T14. Writes
commit to SQLite before the live registry changes, and delete retries converge on
the same state. List responses include live health.

Migration `00002_services.sql` stores each service in the existing `mesh.db`.
`PublicName` is the single source of truth for D15: empty means tailnet-only, and a
nonempty value retains one exact label below `shaulavo.dev`. The apex,
`mesh.shaulavo.dev`, nested names, wildcards, trailing dots, malformed labels,
and names outside the zone are rejected. D16 narrows D15 here: explicit naming
is still required for every eligible public name, but it can never authorize
the apex. The daemon restores all rows on startup.

No dependency was added. Static and file serving use `net/http`; proxying uses
`net/http/httputil`.

`integration/serving_survives_restart.sh` creates a service through the daemon
control socket, kills and restarts the daemon, checks the restored HTTP response,
then removes the root and checks for HTTP 503 while the daemon remains alive.

Verified with:

```bash
go generate ./...
go mod tidy -diff
go vet ./...
go test -race ./...
go test ./internal/serve -run=^$ -fuzz=FuzzResolveRootConfinement -fuzztime=10s
./scripts/verify.sh
```
