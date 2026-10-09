# Public hosting retirement

Status: Approved, 2026-10-09. The owner explicitly requested removal of all public
hosting from Mesh. This supersedes the narrower temporary-app-only retirement.

Mesh retains private services, temporary apps, browser approval, certificates,
terminal sessions and their existing workers. The reusable widget source remains
in `web/app-pill` and `internal/apppill`, with no production dependency.

## Removed contracts

Public service flags and confirmations, gateway listener modes, service and
route publishers, routing snapshots/outboxes, certificate profiles, SSH hostname
claims/reverse tunnels, and public dispatch are deleted. Unknown controls use the
ordinary error path. There are no old field aliases or version-specific repairs.
The source guard runs in CI through `scripts/check-private-hosting-boundary.sh`.

The private app client owns only signed registry exchange and exact Tailnet peer
pinning. The registry accepts loopback TLS with UID-verified PROXY metadata and
verified Tailnet ownership. App proxy requests use the numeric origin endpoint;
the origin restores the browser Host from its verified signed admission.

The existing private-service wildcard certificate serves short private services
and the registry. Renewal distributes to an explicit `appRegistry` recipient
without adding app DNS records. Identical recipients receive one install;
conflicting identity or endpoint declarations fail during configuration.

## State and cutover

Schema 13 drops public routes, publication outboxes and tunnel claims, removes
service exposure fields, and renames the app hostname column. Its forward-only
migration preserves private app bytes, names/tombstones, browser grants,
services, caches, sessions and source files. App allocation still acquires a
SQLite write reservation before reading ownership.

Upgrade origin, registry and gateway executables with their configuration.
The registry uses `--app-registry-config`, the origin uses
`--app-registry-target`, and certificate renewal uses `appRegistry`.
Seed the registry's private-service certificate store with its validated current
wildcard PEM bundle. Preserve existing private-service issuer state and the
origin's machine certificate store. Keep the old certificate/configuration state
in the private deployment backup, outside active state. Reconcile private names
and verify the new signed certificate distribution after restarting.

Release receipts prove a forward state upgrade and candidate restart while the
original worker and saved recovery remain intact. They do not require an older
executable to read the new schema. The changed producer and consumers share the
same receipt contract with no fallback.

## Verification

Migration tests seed populated public state and prove its removal while comparing
retained private rows, opaque app/lease/grant bytes and source hashes. Client,
private ingress, certificate recipient, browser security, HTTP/WebSocket,
source download, app lifecycle, session/SSH and reverse-forward denial checks
exercise the private paths.

Required checks are `go mod tidy -diff`, `go vet ./...`, `go test -race ./...`,
`scripts/verify.sh`, the source guard, and full quality gates. A deployed check
also verifies mapped executable hashes, effective flags, absent public tables,
renewed private leases, private app and named-service responses, unchanged source
hashes and surviving worker PID/start tokens.
