# T14 — `mesh serve`

**Status:** done · **Depends on:** T11, T12, T13, T07 · **Primary owner:** `internal/cli/`

## User interface

```bash
mesh serve pc ./site --at /blog
mesh serve pc 3000 --at /api
mesh serve pi /mnt/data --at /files --files
mesh serve pc ./site --at /blog --public blog.shaulavo.dev
mesh serve ls                         # `serve list` is an alias
mesh unserve /blog
mesh unserve /blog --host pc         # resolve duplicate route names
```

A numeric target is a proxy port. A directory is static unless `--files` is
set. The CLI never infers a public hostname. The user must type the complete
one-label `*.shaulavo.dev` name.

`mesh serve ls` prints `ROUTE`, `HOST`, `KIND`, `TARGET`, `SCOPE`, `HEALTH`, and
`URL`. It queries adopted hosts concurrently under one hard deadline, replaces
the SQLite cache only after a complete live response, and uses cached rows as
`offline/stale` when an origin cannot be reached. It also requests bounded,
paginated edge status when an origin has a configured public-edge publisher.

The preferred private URL uses the canonical private name authenticated through
T12, for example `https://pc.mesh.shaulavo.dev/blog`. The Pi adds that name to
the signed live `private-origin` certificate install only after its A record is
reconciled. The origin persists the name beside the valid live certificate and
returns it from `host.info` only after Tailscale Serve has configured tailnet
port 443. The CLI pins the origin identity before accepting the name. Host
aliases remain display labels and never become URL authority.

If an authenticated private name is not available, the CLI derives a reachable
fallback from the same identity-verified control endpoint: `ws` becomes `http`,
`wss` becomes `https`, and the terminal path is replaced by the service route.
The cached service snapshot retains the last authenticated private name for
offline listing. A private name is stable for the adopted identity; an operator
must reset its private-name state and re-adopt it before an intentional rename.

## Public-directory boundary

The origin daemon resolves relative targets against its own user's home
directory. It returns the canonical path and a bounded file count through
`service.preview`. The CLI shows that path, count, and exact public URL before
it asks for confirmation. `--yes` skips only this prompt.

The daemon repeats resolution and scanning while it holds the service mutation
gate. The upsert includes the prior preview, so a changed canonical target or
file count is rejected before persistence. Public directory traversal is
bounded, follows only symlinks that remain inside the resolved root, detects
cycles, verifies regular files can be opened, and fails closed on I/O errors,
special files, broken or escaping symlinks, depth, entry count, or cancellation.

The scan refuses `.env`, `.env.*`, `.git`, `.ssh`, `id_*`, and `*.pem` names,
including names reached through safe in-root symlinks. Only the explicit
`--allow-credentials` flag bypasses credential-name refusal; it does not bypass
any confinement or resource failure. The interactive confirmation or
noninteractive warning states when this override was selected.

An existing public route cannot be changed to private or moved to another
public hostname in place. Run `mesh unserve` first. This prevents the old edge
claim from briefly exposing a replacement target before withdrawal.

## Deletion and ambiguity

Route-only `mesh unserve /blog` considers live and cached ownership on every
adopted host. It refuses when the route has multiple candidates or when an
unavailable host makes uniqueness unprovable. `--host ALIAS` is the explicit
escape hatch, but the selected host must answer a live authoritative list.

The origin responds with `service.deleted` only after T13 durably acknowledges
the complete reduced snapshot. Therefore CLI success means the public URL has
been withdrawn; retrying an already absent daemon-side delete still republishes
and waits for the same acknowledgement.

## Implementation dependencies

T14 primarily owns the Cobra surface, one-shot host adapters, catalog, and
confirmation UI in `internal/cli/`. The security and durability boundary cannot
live on the client, so this task also adds:

- additive `service.preview` / `service.previewed` protocol fields;
- origin-side inspection and upsert enforcement in `internal/serve/` and
  `internal/daemon/`;
- migration 00004 and storage methods for complete per-host cached service
  snapshots, including the authenticated private name.

The existing reconnecting transport remains exclusive to terminal attachment.
Every preview, mutation, list, and deletion uses a separate non-reconnecting
connection and verifies `host.info` before sending its control request.

## Verification

The retained `integration/serve_cli.sh` builds a real `mesh_integration` binary
and starts real origin and edge daemon processes with loopback Tailscale
fixtures. It covers static, files, and proxy inference; remote-home resolution;
the interactive public confirmation; credential refusal and override; live and
cached-offline listing; daemon restart; exact public acknowledgement; and both
origin and edge 404 after deletion.

`integration/cli_surface.sh` renders help through Fang and proves the mutation,
`serve ls`, and `serve list` forms are shown separately. It rejects the invalid
hybrid form that combines mutation arguments with a subcommand.

The deterministic environment does not mutate public DNS or contact Let's
Encrypt. T12 and T13 retain those operator acceptance checks; T14 reuses their
installed certificates and edge protocol without widening Cloudflare-token
scope.

Repository gates:

```bash
gofmt -l .
go vet ./...
go test -race ./...
bash scripts/verify.sh
```

Executed on 2026-08-30: formatting and tidy diffs were clean, sqlc generation
was idempotent, `go vet ./...` and `go test -race ./...` passed, all 17 retained
integration scripts passed, and the affected command, CLI, daemon, storage, and
serve test binaries cross-compiled for Darwin arm64.
