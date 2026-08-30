# T11 — Serving core

**Status:** not started · **Blocked by:** nothing (T04 landed) · **Owns:** `internal/serve/`

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
    Name     string // route name, e.g. "blog"
    Kind     Kind
    Target   string // directory path, or port for proxy
    Public   bool
    Subdomain bool
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
