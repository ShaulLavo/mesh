# Private app retirement

Approved, 2026-10-09. The owner clarified that temporary apps must have no
public-sharing controls or injected widget. Preserve the UI source for another
project. Issue #81 closed by transferring work to Fregat Plan 291; that closure
did not ship the removal.

## Design decision

Preserve the origin-owned files, workers, update intents, receipts and signed
lifecycle exchange with the existing registry. Remove temporary-app dispatch
from the ordinary public registry. A separate dispatcher requires the validated
TLS/PROXY listener, matching SNI and Host, a Tailnet source address, and a mapped
Tailnet owner before any browser cookie or app handler is considered.

Origin-local registries were considered. They would require DNS and collision
coordination for the existing four-character URLs, plus importing authoritative
pending-operation replies. Clearing foreign-edge pending operations could lose
a delete or revision. That migration adds risk without helping retire sharing.

Name reservations remain as private hostname collision guards and tombstones.
The `apps.edge` durable key and signing domains preserve lifecycle replay
semantics. They do not register app routes in the public HTTP registry.
The control operation is now `app.registry`; old `app.edge` is rejected.

The generic Solid component stays in `web/app-pill` and exports `FloatingPill`
and `mountFloatingPill`. Callers provide actions and mount explicitly. Mesh has
no production dependency on the preserved HTML transformer or widget assets.

## Storage boundary

Private app state stays in `private_app_state`. The registry keeps app-name
reservations and inactive tombstones. Public route, snapshot, outbox and tunnel
tables are removed. App allocation reserves a database write before checking
names, then commits names and app state together.

The private app registry uses a dedicated loopback TLS listener with authenticated
Tailnet PROXY ingress. Its live wildcard certificate uses the private-service
profile shared with named private services. Renewal configuration names the
registry with `appRegistry`; it does not write a DNS record for that recipient.

## Verification

Run the source boundary check and required repository checks:

```sh
scripts/check-private-app-boundary.sh
go mod tidy -diff
go vet ./...
go test -race ./...
./scripts/verify.sh
```

The app server and update integrations use real HTTPS, a signed certificate
installation, authenticated PROXY metadata, actual Tailnet owner lookup,
HTTP servers and WebSockets. They also cover static bytes, source downloads,
renewal, registry/origin restarts, expiry, rejected sharing commands and outside
clients with forged headers and cookies.

The generic widget's
browser checks cover Chromium and WebKit, normal and reduced motion, touch and
mouse input, every docking edge, viewport changes and explicit mount/disposal.
