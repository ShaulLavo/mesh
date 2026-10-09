# Deployment domains

Mesh is Tailnet-only on `sprockt.dev`. The owner removed public temporary apps,
public serving and VPS-edge tunnels on 2026-10-08. Public hosting belongs to
Brine on `shaulavo.dev`. Mesh has private-origin and private-service certificate profiles.

Mesh reads `domains.json` beside `hosts.json` before the interactive picker and
ordinary client commands start.
`MESH_CONFIG_DIR` selects that directory. Otherwise Mesh uses
`$XDG_CONFIG_HOME/mesh` or `~/.config/mesh`.

```json
{
  "primary": "new.example",
  "aliases": ["old.example"],
  "legacyCertificateDomain": "old.example"
}
```

The primary domain supplies new app URLs and the default renewal domain. Aliases
accept existing app and management URLs. Private names work on each accepted
domain after its own signed certificate and name have been installed.
Configuration is immutable during a process lifetime. Restart each affected
process after changing it.

Existing installations upgrade automatically when this file is missing. Startup
reads the deployment domain from the existing private name and certificate
stores, then writes a mode-0600 policy with that domain as both `primary` and
`legacyCertificateDomain`. This keeps the existing URLs and certificate paths.
The migration preserves any policy already on disk, including one published by
a concurrent startup. When per-domain certificate slots already exist and the
policy is missing, startup stops with a recovery error. Restore `domains.json`
from the deployment configuration backup before starting ordinary commands.
`mesh update` remains available during recovery.

An existing catalog or address book uses the pre-policy release domain when
certificate and name state are absent. That compatibility default is
`shaulavo.dev`. Existing private-name and certificate state takes precedence.
A fresh installation that requests deployment names defaults to `sprockt.dev`
without a legacy certificate domain. Configure `domains.json` for another domain.

A fresh native-only installation works with no policy. Detached workers, agent
helpers, native device enrollment, version inspection, and update helpers stay
independent of this file. Session listing through `mesh ls` or `mesh list` loads
the configured policy or infers legacy names in memory. Listing keeps deployment
configuration and state files unchanged, including read-only configuration
directories.
Invalid policy stops ordinary commands before they start a listener or validate
service caches. Managed updates and direct executable replacement both migrate
on the new daemon's startup.

Use canonical lowercase DNS names. Mesh accepts at most eight domains and rejects
duplicates, parent/child overlaps, IP addresses, unknown JSON fields, and files
larger than 64 KiB. These domains control acceptance only. They grant no app owner
permissions and perform no DNS writes by themselves.

## Certificate slots

Private-origin and private-service profiles have separate stores for every
accepted domain. Machine certificates cover `*.mesh.<domain>`; service and app
registry certificates cover `*.<domain>`. SNI chooses the matching slot. An
unknown SNI is rejected.

For an existing installation, `legacyCertificateDomain` identifies the domain
whose installed certificates and renewal state already occupy the original
store paths. That domain keeps those paths even when it becomes an alias. Other
domains use `domains/<domain>` below their profile root. Keep this field unchanged
through the overlap. When retiring that domain, remove both its alias and this
field. Preserve the old certificate stores in the deployment backup.

Live and staging stores remain separate. Staging certificates never become live
HTTP names. Every installed private domain keeps the same pinned host label,
such as `desktop.mesh.new.example` and `desktop.mesh.old.example`. A different
label is rejected, including concurrent installs. Signer and target identities
are still checked for every bundle.

## Private app registry

Start the registry daemon with `--app-registry-config /absolute/path/apps.json`.
Its strict configuration declares a loopback TLS listener and origin peers:

```json
{
  "listenAddress": "127.0.0.1:8445",
  "certificateRenewerId": "RENEWER_IDENTITY",
  "origins": [
    {
      "identity": "ORIGIN_IDENTITY",
      "tailscaleName": "desktop.example.ts.net",
      "controlPort": 7337,
      "websocketPath": "/mesh"
    }
  ]
}
```

The listener requires authenticated Tailnet PROXY ingress. It serves known app
and management hosts after resolving the Tailnet owner. Ordinary services stay
on their origin's private listener. Configure the origin's
`--app-registry-target` with the registry identity, Tailscale name, control port
and WebSocket path. The registry certificate uses the private-service install
slot even when the registry has no private-origin HTTPS listener. If one daemon
runs both roles, both roles must pin the same certificate renewer.

## Renewal for both zones

Each private-name renewal file may set `domain` and `additionalConfigs`:

```json
{
  "domain": "new.example",
  "zoneDomain": "new.example",
  "additionalConfigs": ["/absolute/path/private-names-old.json"],
  "zoneId": "NEW_ZONE_ID",
  "tokenFile": "/absolute/path/new-zone.token",
  "acmeEmail": "owner@example.com",
  "directoryUrl": "https://acme-v02.api.letsencrypt.org/directory",
  "acceptTerms": true,
  "origins": [
    {
      "name": "desktop",
      "tailscaleName": "desktop.example.ts.net",
      "identity": "ORIGIN_IDENTITY",
      "controlPort": 7337,
      "websocketPath": "/mesh"
    }
  ]
}
```

The second file uses `domain: "old.example"`, `zoneDomain: "old.example"`, and
that zone's ID and token path.
Configure accepted domains on the renewer and every recipient before selecting
new-zone credentials. During migration, set an explicit `domain` in both renewal
files, including the existing one, before changing the policy's primary. An
omitted domain inherits the primary; changing credentials alone does not change
that domain. Preserve the old renewal file and token during overlap, and use a
separate destination token path. Keep `legacyCertificateDomain` unchanged until
the old domain is safely retired.
For private app HTTPS, add an `appRegistry` recipient to each renewal file:

```json
"appRegistry": {
  "tailscaleName": "registry.example.ts.net",
  "identity": "REGISTRY_IDENTITY",
  "controlPort": 7338,
  "websocketPath": "/mesh"
}
```

The registry receives the same private-service wildcard as service origins.
This recipient creates no A records. A recipient-only renewal file can omit
`origins`. Replace identity and zone placeholders before running the command.

Each zone gets its own DNS solver, ACME account state, renewal loop, and signed
certificate distributor. `private-names reconcile` processes all configured
zones. Daemon renewal runs the loops concurrently. The combined graph permits
at most eight files, one per domain, and rejects repeated paths and cycles.
Tokens remain separate exact-0600 files. No live DNS or ACME operation runs
until the configured renewal command or daemon starts it.

## Renewal credential bindings

Set `zoneDomain` in new renewal configurations to the domain served by their
Cloudflare zone ID and token. It is an optional flat JSON string, separate from
`domain`. Mesh requires it to match the effective renewal domain: the explicit
`domain`, or the policy primary when `domain` is omitted. Mesh performs this
check locally; it makes no provider zone-discovery calls. DNS-Write-only tokens
remain supported. The declaration is the operator's confirmation of the zone,
not proof that Cloudflare maps a given zone ID to that name.

On the first renewal runtime setup, Mesh saves the effective domain and
zone ID for that absolute renewal-config path. Bindings live below
`<StateDir>/private-names/zone-bindings`, in mode-0600 files within a mode-0700
directory. Subsequent changes to the domain or `zoneId` are refused unless an
explicit `zoneDomain` matches the effective domain and authorizes rebinding.
A token change alone keeps the same domain and zone ID. Preserve this state
with deployment backups; a new config path has no earlier binding.

Readers and writers use the same per-binding cross-process lock. Mesh resolves
symlinked state-directory ancestors to the physical binding directory before
locking and publication. Publication syncs a temporary file in that directory,
renames it into place, then syncs the physical directory and its ancestors
through the filesystem root before setup succeeds. Existing identical bindings also pass file and directory sync barriers
under that lock, repairing an earlier interrupted publication. A sync error
stops renewal setup; another initializer cannot accept a visible file while its
publisher is still completing the durability barriers. Keep the stable lock
file in place across retries and process restarts.

**First-upgrade limitation:** a legacy configuration with no `zoneDomain` and no
saved binding is accepted and records its current pair. If its zone ID was
already swapped before the first runtime setup, Mesh cannot detect that mismatch
without provider discovery. Inspect existing domain/zone pairs and set matching explicit
`domain` and `zoneDomain` values before changing credentials. Keep the old zone's
renewal and token in their existing files during overlap; create a separate
configuration and credentials for the destination.

### Recovering damaged binding state

A corrupt or unreadable binding stops renewal setup, including explicit
rebinding. Restore the indicated binding file from a known-good deployment
backup with its original owner and mode `0600`.

If no usable backup exists, recovery is an explicit operator action:

1. Stop every renewal process sharing this state directory. Preserve the current
   configuration and the damaged file named by the error. Keep lock files in
   place; deleting a lock file can let processes acquire different locks.
2. Independently verify the intended DNS zone name, its zone ID and the token's
   scope. Check that the renewal domain is accepted by the deployment policy.
   Keep old aliases and `legacyCertificateDomain` during migration. Do not infer
   the intended zone from the damaged binding or an inherited primary alone.
3. Set explicit matching `domain` and `zoneDomain` in the renewal configuration,
   with the verified zone ID and token. Archive only the indicated damaged
   binding outside `zone-bindings`; keep the archive for diagnosis. Do not
   remove the binding directory, other bindings, lock files or certificate state.
4. Start the configured renewer and confirm setup succeeds before using the
   resulting DNS records or certificates. Keep the corrected configuration and
   newly recorded binding in the deployment backup.

Archiving a binding removes its history. This procedure requires independent
zone verification and the explicit declaration; it is not an automatic retry
or a way to bypass an unexplained credential mismatch.

## Private service hosts

A served route can have a private hostname at its root:

```sh
mesh serve desktop 3301 --at /platform --private-host fregat --isolate
```

With `primary: "new.example"`, this publishes `https://fregat.new.example/`.
`--private-host` accepts a label or a full hostname in an accepted domain. When
`--at` is omitted, the hostname label becomes the route key. `mesh serve ls`
shows the root hostname. The original machine path returns 404, including API
and WebSocket requests. The route key still identifies the service for commands
such as `mesh serve stop /platform` and `mesh unserve /platform`.

The service stays on the origin's tailnet listener. Existing proxy, files,
static, on-demand, and isolation
options apply to the root hostname too. Cross-origin requests use the same
private-service checks as path mounts. A hostname belongs to one service. `mesh`, `apps`, and four-character temporary
app identifiers are reserved.

For an existing route, configure `serviceNames` first. Reconcile its owned DNS
record, install the private-service certificate, and check the hostname through
the deployment gateway. Then publish the route with `--private-host`. Publishing
retires the original machine path, so verify DNS and the certificate before
changing an existing route.

Configure DNS ownership separately on the certificate renewer. Add
`serviceNames` to the corresponding origin in its private-name JSON file:

```json
{
  "name": "desktop",
  "serviceNames": ["fregat"],
  "tailscaleName": "desktop.example.ts.net",
  "identity": "ORIGIN_IDENTITY",
  "controlPort": 7337,
  "websocketPath": "/mesh"
}
```

The renewer creates unproxied A records pointing to that origin's Tailscale IPv4
address. It installs a `*.<domain>` certificate in the origin's separate
`private-service` slot, signed by the pinned renewer for the exact origin
identity. Only private-service signed bundles can install into that slot. Existing
private-origin certificate stores and `*.mesh.<domain>` names stay unchanged.
Each service origin holds the private key for the domain's wildcard certificate.
Trust those origins to protect every name in that domain, including private app
names. Profile pins protect certificate installation; they do not narrow what a
wildcard certificate can authenticate.
Restart the configured renewer daemon or run `mesh private-names reconcile`
with its existing config and explicit `--live --accept-tos` flags to apply the
new names. The CLI flag alone does not create DNS records.
Re-running `mesh serve` preserves the route's private hostname when the flag is
omitted. Use `--private-host=` to clear it explicitly. Removing a served
route removes its HTTP hostname; separately remove its `serviceNames` entry
and DNS record when retiring the name.

The TLS gateway needs the deployment's `--domains` policy and forwards these
names to the private origin listener. It has no service-specific proxy table.

Old links on the short host, such as `/platform/chat?id=1`, redirect to
`/chat?id=1` with temporary status 307. The original machine path is retired
for pages, API requests and WebSockets. Browser storage and pairing belong to
an origin, so opening the short hostname may need pairing.

## App overlap

An app ID reserves its name in every accepted domain under the same owner.
Creation writes those reservations and app state in one transaction. Alias
collisions roll back the whole creation. Startup adopts aliases for existing
active apps under their recorded owner and stops on a conflicting reservation.
Retirement marks every configured alias inactive.

Requests keep their own domain for management pages,
pairing redirects, origin checks, and return URLs. A return URL must name the same
app ID on an accepted domain. Private view challenges set the browser nonce on
the destination app domain, then return to the authenticated manager to issue
the ticket. The challenge return host must match the app host that sets the
nonce. Private browser grants require currently valid, installed live private-service
certificates for both the app and its manager. Alias configuration and staging
certificates alone leave grants disabled. Certificate renewal enables grants
without restarting the registry. The app registry uses a loopback TLS listener
behind authenticated Tailnet PROXY ingress. Browser cookies remain host-only. Parent-domain cookies are stripped
for every accepted domain. Signed admissions retain their target hostname, URI, method,
owner identity, and generation checks.

## Roll out without losing remote access

1. Back up affected configuration files and inspect DNS in both zones.
2. Deploy this source and check the policy created for the existing domain.
3. Add the destination alias on origins, app registries, renewers, and gateways.
4. Install destination DNS and certificates while keeping old renewals active.
5. Check both domains for private services and disposable apps.
6. Change the primary, then update consumers after both paths work.
7. Retire only old Mesh records after all owner machines and URLs have passed.

Build the optional gateway with its `--domains` path. It must use the same accepted
domains as its backends. See [the gateway guide](../examples/tailnet-gateway/README.md).
Deployment changes require credentials for both zones during overlap. Preserve
unrelated records, including the old apex and website routes. Source changes
alone do not migrate a running installation.

## Fregat usage feed

Fregat owns the current cached feed at `/providers/usage/feed` inside its base
path. With Fregat served at `/platform`, configure Mesh's dashboard feed URL as
`https://desktop.mesh.new.example/platform/providers/usage/feed`.
The former CLIProxyAPI-produced `/ai-usage/v1.json` route was retired during the
Fregat feed migration. A dashboard still pointing there needs its consumer URL
updated after the replacement is checked. Mesh continues to consume the v1 feed
schema.
