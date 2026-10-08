# Deployment domains

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

An existing catalog or address book, or a command that enables deployment names,
uses the pre-policy release domain when certificate and name state are absent.
That historical default is `shaulavo.dev`. Configure `domains.json` to choose
another domain for a new deployment.

A fresh native-only installation works with no policy. Detached workers, agent
helpers, version inspection, and update helpers stay independent of this file.
Invalid policy stops ordinary commands before they start a listener or validate
service caches. Managed updates and direct executable replacement both migrate
on the new daemon's startup.

Use canonical lowercase DNS names. Mesh accepts at most eight domains and rejects
duplicates, parent/child overlaps, IP addresses, unknown JSON fields, and files
larger than 64 KiB. These domains control acceptance only. They grant no app owner
permissions and perform no DNS writes by themselves.

## Certificate slots

Private and public profiles have separate stores for every accepted domain.
Private certificates cover `*.mesh.<domain>`; public edge certificates cover
`*.<domain>`. SNI chooses the matching slot. An unknown SNI is rejected.

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

## Renewal for both zones

Each private-name renewal file may set `domain` and `additionalConfigs`:

```json
{
  "domain": "new.example",
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

The second file uses `domain: "old.example"` and that zone's ID and token path.
Use the existing `publicEdge` recipient field in each file when distributing
public certificates too. The example's identity and zone placeholders must be
replaced before running it.

Each zone gets its own DNS solver, ACME account state, renewal loop, and signed
certificate distributor. `private-names reconcile` processes all configured
zones. Daemon renewal runs the loops concurrently. The combined graph permits
at most eight files, one per domain, and rejects repeated paths and cycles.
Tokens remain separate exact-0600 files. No live DNS or ACME operation runs
until the configured renewal command or daemon starts it.

## App overlap

An app ID reserves its name in every accepted domain under the same owner.
Creation writes those reservations and app state in one transaction. Alias
collisions roll back the whole creation. Startup adopts aliases for existing
active apps under their recorded owner and stops on a conflicting reservation.
Retirement marks every configured alias inactive.

Requests keep their own domain for the injected app controls, management pages,
pairing redirects, origin checks, and return URLs. A return URL must name the same
app ID on an accepted domain. Private view challenges set the browser nonce on
the destination app domain, then return to the authenticated manager to issue
the ticket. The challenge return host must match the app host that sets the
nonce. Private browser grants require currently valid, installed live public-edge
certificates for both the app and its manager. Alias configuration and staging
certificates alone leave grants disabled. Certificate renewal enables grants
without restarting the edge. Proxy mode keeps grants disabled because its
external TLS terminator has no installed certificate slot in Mesh; use the
direct-TLS listener for private browser views. Public apps and Tailnet owner
access keep their existing behavior. Browser cookies remain host-only. Parent-domain cookies are stripped
for every accepted domain. Signed admissions retain their target hostname, URI, method,
owner identity, and generation checks.

## Roll out without losing remote access

1. Back up affected configuration files and inspect DNS in both zones.
2. Deploy this source and check the policy created for the existing domain.
3. Add the destination alias on origins, app edges, renewers, and gateways.
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
