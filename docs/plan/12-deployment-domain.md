# Deployment domain and parallel cutover

Status: Approved, 2026-10-08.

Owner decision: "We can move the mesh to sprockt." Mesh names move from `shaulavo.dev` to `sprockt.dev`. The `shaulavo.dev` apex belongs to the owner's production website on the Hetzner VPS. This work must preserve that record and the website lane's web stack.

This plan executes the domain part of [#241](https://github.com/ShaulLavo/mesh/issues/241), retained in [11-session-contract-and-boundaries.md](11-session-contract-and-boundaries.md). It preserves all six session invariants in `CLAUDE.md`.

## Current policy, 2026-10-08

Mesh is Tailnet-only on `sprockt.dev`. Public temporary apps, public serving and
VPS-edge reverse tunnels are removed. Public hosting belongs to Brine on
`shaulavo.dev`; Mesh must preserve that zone’s website records and services.
The owner’s current private service URLs are `https://fregat.sprockt.dev/`,
`https://ai.sprockt.dev/` and `https://comfy.sprockt.dev/`. Their old machine-path
aliases are retired. The dated inspection and original cutover sequence below
remain evidence of the starting state, not an inventory of today’s deployment.
Use [deployment-domains.md](../deployment-domains.md) for current configuration.

## Execution state — baseline recorded 2026-10-08

- [x] Inventory source, local configuration, authoritative DNS, deployed certificates and active routes.
- [x] Test existing Cloudflare token visibility without exposing the token.
- [ ] Obtain access to the destination zone. This is the first blocked gate.
- [ ] Implement configuration-driven naming and parallel certificate/name support with tests.
- [ ] Issue destination certificates and add destination DNS records while old records remain.
- [ ] Verify every destination route before updating callers.
- [ ] Back up and update owner configuration and operating instructions.
- [ ] Verify owner-machine callers, then retire only the old Mesh records.

At the baseline inspection, no live configuration, DNS records, certificates, binaries or services had changed. On 2026-10-08, the existing token returned the active `shaulavo.dev` zone from `GET /client/v4/zones?name=shaulavo.dev`. The same request for `sprockt.dev` returned `success: true` with an empty result. Shared nameservers do not establish token access to both zones.

Required credential: a Cloudflare API token with Zone Read and DNS Edit for `sprockt.dev`. Retain access to `shaulavo.dev` during overlap, either on the same token or through a separate zone-specific token. Verify the destination zone ID and read its records before writing anything. Do not commit the token, account details or credential file contents.

## Inventory — baseline recorded 2026-10-08

Source inspection used Mesh main `ae78899508ace2bf37be48101b04fda5c7275e17`. Operator paths below identify the existing deployment; they must not become defaults in repository commands.

### Source and callers

| Location | Current dependency | Required change |
| --- | --- | --- |
| `internal/serve/service.go` | `PublicDomain` and public hostname validation | Read validated deployment policy; retain label reservations and canonicalization. |
| `internal/dnsname/records.go` | `Zone`, `PrivateZone`, `WildcardName`; private A record generation | Derive names from the configured zone. Preserve exact record-ownership checks. |
| `internal/dnsname/distribution.go` | `PublicWildcardName`; profile-to-name validation and signed certificate installation | Bind profile, zone and destination identity together. Support both zones during cutover. |
| `internal/dnsname/private_name.go` | Private hostname validation; persisted name pinned to one value | Preserve the old name while adopting the new name. A reset of the live origin is not a cutover mechanism. |
| `internal/dnsname/config.go` | One Cloudflare zone ID/token and ACME runtime | Pair a DNS zone name with its provider credentials; isolate ACME state by zone and environment. |
| `internal/dnsname/acme.go`, `manager.go`, `public_manager.go` | Wildcard defaults, renewal and distribution | Use the same validated domain policy as listeners and records. |
| `internal/daemon/certificates.go` | Single profile-specific expected name and certificate slot | Select certificates by SNI with explicit old/new slots or verified SAN coverage. Keep old renewal active until retirement. |
| `internal/apps/types.go` | `Domain`, `ManagementHost`, `ManagementOrigin`, generated app URL | Derive public and management names from policy. Keep app IDs, owner keys and expiry unchanged. |
| `internal/apps/edge.go`, `http.go`, `origin.go` | Stored app names, admissions, host/return URL validation, management HTML | Accept and validate both domains during overlap; publish the configured primary URL. Preserve signed host binding and pairing protections. |
| `internal/apppill/pill.go` | App host in CSP and injected page control | Derive the host from the request's validated domain. Test both origins and manager origins. |
| `internal/edge/cookies.go` | Parent-domain cookie stripping | Strip parent-domain cookies for every accepted deployment domain. |
| `internal/cli/serve_command.go`, `serve_claim.go` | Help strings containing a personal domain | Use neutral examples or the configured domain. |
| `examples/tailnet-gateway/main.go` | SNI dispatch for `apps.shaulavo.dev` and four-character app labels | Load the same policy or explicit domain arguments. Dispatch both zones to the proper backends during overlap. |
| `examples/tailnet-gateway/README.md` | Deployment instructions | Document configuration and parallel rollout. |

Inventory command, run from the checkout:

```sh
rg -n 'shaulavo\.dev|sprockt\.dev' internal examples integration web docs
rg -n 'PublicDomain|PublicWildcardName|PrivateZone|WildcardName|ManagementHost|ManagementOrigin' internal examples
```

The first command also finds domain fixtures in `internal/{serve,daemon,dnsname,apps,apppill,edge,cli,tunnel,storage,protocol,privacy,tui}`, `examples/tailnet-gateway` and `web/app-pill/tests`. Integration callers include `serve_cli.sh`, `reverse_tunnels.sh`, `temporary_app_server.sh`, `temporary_app_update.sh`, `public_edge.sh` and `private_tls_distribution.sh`. Move executable fixtures to an explicit test domain with per-test configuration. Test isolation must not depend on an owner's configuration file.

Historical documentation occurs in plans 00, 01, 03, 04, 06, 10 and 11; task documents T11 through T18 and T29; and the temporary-app, serve-on-demand, reverse-tunnel, remote-access and recovery guides. Update active instructions. Keep historical evidence labelled with the domain it actually measured.

### Owner configuration and services

| Location or machine | Observed state | Cutover action |
| --- | --- | --- |
| Omarchy, also displayed as `pc` | Tailnet IPv4 `100.77.94.94`; Mesh daemon on 7337, private HTTPS backend on 8443 | Keep identities, PTYs, grants and listener ports. Add new DNS/TLS names. |
| `~/.config/mesh/private-names-live.json` | Old zone ID, separate `cloudflare.token`, live Let's Encrypt URL, Omarchy origin and local app edge | Back up, then configure both zone credentials and isolated renewal state. |
| `~/.config/mesh/apps-edge-target.json` | Existing edge identity and endpoint on 7338 | Preserve identity and transport address. No domain appears in this file. |
| `~/.config/mesh/hosts.json` | Mac, Omarchy, Pi and VPS identity/address records; dashboard feed uses old HTTPS name | Preserve the address book. Change only the feed URL after it returns the expected JSON. |
| `mesh.service` and drop-ins | Origin, renewer, Tailscale Serve forwarding to 8446; `KillMode=process` | Preserve worker ownership and forwarding. Restart only at a quiet moment and check health immediately. |
| `mesh-demo-edge.service` | Edge on 7338; `MESH_STATE_DIR` points to `/work/mesh/demo-edge`; TLS backend on 8445 | Back up edge config, preserve the database and app identities, add parallel domain support. |
| `mesh-demo-gateway.service` | Gateway binary at `/work/mesh/demo-edge/tls-gateway`; SNI router on 8446 | Replace only after dual-domain routing tests. It forwards encrypted connections; TLS terminates at backends. |
| `/work/mesh/demo-edge/preview.py` | Redirects to `5yfw.shaulavo.dev` | Inventory whether this helper is still used; update only if active. |
| `~/.platform/settings.json` | `developer.deployTarget.meshOrigin` uses old name | Copy to `settings.json.bak-<timestamp>` before editing. Preserve every other setting and `meshHost`. |
| `platform-prod.service` | `SERVER_ALLOWED_ORIGINS` includes the old HTTPS origin | Add both origins before browser checks. Preserve the service and terminal processes. |
| `~/.agents/skills/fregat-local/SKILL.md` | Old owner-instance, deploy-target and verification URLs | Back up, then update after successful new-domain checks. |
| `~/.agents/AGENTS.md` | No exact old-domain match in this inspection | Recheck before completion. Preserve symlinks and unrelated instructions. |
| Production release records | Historical `live-check.json` records contain old URLs | Preserve historical records. Locate the active page/launch record separately and back it up before changing it. Its exact location remains unconfirmed. |
| Mac | Address book has a native control endpoint and identity | Remote configuration, certificates and browser/launch records are unverified. Coordinate access with the terminal-performance wave; do not run commands there while reserved. |
| Pi | Address book has a native control endpoint and identity | Remote renewer/service configuration remains unverified. The inspected Omarchy service currently runs the configured renewal runtime locally. Do not assume the Pi is the active renewer. |
| Hetzner VPS | Address book has a native control endpoint and identity | Remote Mesh edge, origin and certificate configuration remains unverified. Coordinate with the website lane. Leave the apex, website services and VPS web stack untouched. |

Back up each owner configuration file before editing, preserving modes and ownership. Keep a manifest of old/new values and backup paths in the operator's evidence directory. Do not copy secret files into reports. Use credential-file references.

### Authoritative DNS and certificates

The accessible Cloudflare zone contained these records on 2026-10-08. Listing records is read-only.

| Name | Type and address | Ownership comment | Disposition |
| --- | --- | --- | --- |
| `omarchy.mesh.shaulavo.dev` | A, `100.77.94.94`, DNS-only, TTL 60 | `mesh:private-origin` | Keep until `omarchy.mesh.sprockt.dev` and all its routes verify. |
| `apps.shaulavo.dev` | A, `100.77.94.94`, DNS-only, TTL 60 | `mesh:tailnet-demo` | Keep until new manager and pairing flows verify. |
| `5yfw.shaulavo.dev` | A, `100.77.94.94`, DNS-only, TTL 60 | `mesh:tailnet-demo` | Keep until the corresponding app verifies on the new name. |
| `*.shaulavo.dev` | A, `100.77.94.94`, DNS-only, TTL 60 | `mesh disposable apps (mesh#103)` | Keep until every active app verifies on the new wildcard. |
| `shaulavo.dev` | A, `37.27.17.186`, DNS-only, TTL 300 | `VPS application hosting` | Website lane owns this. Never edit or delete it. |

No private Mac, Pi or VPS DNS record appeared in this zone listing. Their native address-book entries do not prove private HTTPS names exist.

Installed private certificate SAN is `*.mesh.shaulavo.dev`, valid until 2026-12-03. Installed app-edge SAN is `*.shaulavo.dev`, valid until 2026-12-29. Neither covers any `sprockt.dev` name. The wildcard certificates also do not cover the apex. Private certificate state is under `~/.local/state/mesh/private-tls/live`; public edge state is under `/work/mesh/demo-edge/certificates/public-edge/live`.

Inspect destination DNS before deciding which records to add. Preserve unmanaged destination records. DNS-only tailnet A records are the observed deployment model; this decision does not authorize public exposure of private services.

## Configuration contract — current names, private access

Model deployment names as validated policy, separate from host/session identity. The configuration supplies a primary deployment zone, its derived private namespace, and explicit additional accepted zones during migration. Credential configuration pairs each managed zone name with its zone ID and token-file reference. Defaults come from operator configuration, with personal domains removed from core source. An unconfigured machine must still create and attach native terminal sessions.

Reject uppercase, wildcard, URL, IP, empty-label and malformed zone inputs at the configuration boundary. Derive private app names, management names, private origin names, DNS challenges and certificate requirements from that same policy. Keep one-label app validation, reserved labels and private tailnet address restrictions. Avoid mutable global naming changes after listeners start.

Do not deploy a primary-domain-only patch against this live state. The existing certificate slots and persisted private name support one domain; merely replacing a constant would invalidate old certificates, pinned private names and app host checks. Parallel acceptance must be implemented and verified first. Name pinning may accept a second domain for the same origin only through explicit migration policy; unrelated renames still fail.

## Original cutover order and checks — baseline recorded 2026-10-08

The machine-path URLs and public-edge terminology in this original checklist
are superseded. Current service checks use the short private roots above.
Retained app routing components do not grant public access.

1. Obtain and verify destination zone access. Capture destination DNS inventory. Leave both DNS zones untouched if credentials are missing or insufficient.
2. Implement the configuration contract in an isolated worktree. Add failing tests before implementation for a non-personal domain. Cover DNS records, private-name pinning, certificate SAN/profile validation, URL generation, app admissions, CSP, redirects, parent cookies and gateway SNI dispatch. Run focused race tests, `go vet`, and relevant integration scripts through host heavy-job admission. Use fixture DNS/ACME, with no live-provider calls in tests.
3. Back up owner configs, active launch/page records, service definitions and served-route snapshots. Record existing active terminal IDs without attaching, killing or altering them. Confirm which renewer and edge run on each owner machine. Coordinate Mac and VPS access with their owning lanes.
4. Stage old/new certificate acceptance, gateway SNI routing and both Fregat allowed origins. Deploy only after isolated verification. Use worker-preserving daemon restart semantics. Check the old Fregat route immediately after each service change. Never stop worker scopes or other sessions' processes.
5. Obtain staging certificates for `*.mesh.sprockt.dev` and `*.sprockt.dev`. Verify DNS-01 creation and cleanup touch only the intended zone and challenge ownership. Obtain live certificates and install them in separate slots or with explicitly verified multi-zone SAN coverage. Keep old certificates usable and their renewal active.
6. Add destination A records for the existing origin, manager and wildcard apps. Keep all old A records. Verify TLS and responses using the new SNI and Host; a DNS alias alone is insufficient. Check the old names in the same run.
7. Verify `https://omarchy.mesh.sprockt.dev/platform/`, release metadata and a read-only browser load, including assets, API requests, streams and origin checks. Verify every active private route from `mesh serve ls`, including `/ai` and `/comfy`; port-only local routes remain local. Do not invoke a model provider through `/ai`.
8. Verify `https://apps.sprockt.dev` and every active `<id>.sprockt.dev` route. Exercise fixture-backed pairing/admissions, owner controls, WebSockets, injected page controls and cookie/CSP boundaries. Keep private visibility and existing owners. Report any login/pairing step a remote browser still needs before retiring its old route.
9. Restore or establish the expected usage feed, then verify `https://omarchy.mesh.sprockt.dev/ai-usage/v1.json` returns the expected schema and current data. The old feed URL returned HTTP 404 in the baseline, and `/ai-usage` was absent from the current local route list. This is an existing gap, not proof of a migration regression. It must be resolved before claiming feed migration complete.
10. Once all destination checks pass, back up and update Fregat's deploy target, dashboard feed, active page/launch record and local skill URLs. Keep both domains accepted. Verify the owner's Mac/phone-facing URL and confirm all configured owner-machine callers have switched. Capture old/new route status and TLS SAN evidence in the same verification run.
11. Retire old Mesh records only after every required new route and caller passes. Match each exact name, type, address and ownership comment against the recorded inventory. Re-list records immediately before deletion. Stop if ownership or content differs. Never select the apex or records added by the website lane. Keep config backups, migration evidence and old certificates until rollback is no longer needed.
12. Remove old-domain listener acceptance and renewal after retirement is confirmed. Delete obsolete migration APIs/config acceptance as one source change, with retained-primary regression tests. Update current guides, leaving historical evidence intact.

## Rollback and completion — original baseline obligations

At each live step, failure means stop advancing. Preserve or restore the old listener, certificate and gateway path before changing any caller. Restore only this lane's backed-up configuration values, never another session's later changes. Destination records can remain unused while old names serve traffic. Do not delete the old DNS to force adoption.

At the baseline inspection, the intended Fregat URL was `https://omarchy.mesh.sprockt.dev/platform/` and the verified URL was `https://omarchy.mesh.shaulavo.dev/platform/`. Both machine-path entry points are now retired; the current private URL is `https://fregat.sprockt.dev/`.

Completion requires a per-route old/new verification matrix, TLS SANs, DNS ownership evidence, preserved terminal IDs, backup manifest, caller checks and the exact retired records. Until those checks pass, all four inventoried old Mesh A records remain. The `shaulavo.dev` apex stays permanently with the website lane.
