# T12 — Private names and certificates for `mesh.shaulavo.dev`

**Status:** complete · **Blocked by:** nothing (T11 and T06 landed) · **Owns:**
`internal/dnsname/`

## Goal

`https://pc.mesh.shaulavo.dev/blog` works from a tailnet device with a
publicly trusted certificate. The request travels directly to the origin. An
internet client can resolve the name to a tailnet address but cannot route to
it.

## Architecture

Cloudflare is authoritative for `shaulavo.dev`. One zone-scoped DNS Write API
token lives on the always-on Pi. Other origins never receive that token. The Pi
reconciles an unproxied A record for each configured origin, obtains one
`*.mesh.shaulavo.dev` certificate from Let's Encrypt with DNS-01, and sends the
certificate to identity-pinned origin daemons.

The DNS reconciler accepts only IPv4 addresses in Tailscale's
`100.64.0.0/10` range. It owns a record only when the Cloudflare record comment
is exactly `mesh:private-origin`. Record comments are available on Cloudflare's
Free plan; DNS record tags are not. An existing A record with any other comment
is a collision, and Mesh refuses to modify it. DNS-01 records use the exact
comment `mesh:acme-dns01`. Cleanup deletes only the provider ID that Mesh
created and only while that comment remains unchanged. Other TXT values at the
same owner remain intact.

Cloudflare requests use the current v4 record REST API with a Bearer token.
The adapter rejects redirects and malformed base URLs, bounds responses, checks
`success: false` even on HTTP 2xx, and removes the exact token from provider
error text. It uses no Cloudflare SDK.

The ACME issuer uses the RFC 8555 order API in `golang.org/x/crypto/acme`, not
the deprecated certificate API. It presents the TXT value, waits until every
authoritative nameserver reports it, accepts the challenge, waits for the
authorization and order, then cleans the exact record. One issuance has a
10-minute deadline. A separate 15-second cleanup deadline still runs after
cancellation so a failed order does not strand a challenge record.

The Pi stores distinct 0600 ACME account and certificate keys. Certificate
versions and their 0600 current pointer are published atomically. Live and
staging use separate account, key, and bundle directories. A context-aware
per-environment filesystem lock covers load, issuance, and install, so the
unattended daemon and a manual reconciliation cannot create competing orders or
corrupt the current pointer.

Each origin pins the Pi's Mesh Ed25519 identity. Distribution first calls
`host.info` over the origin's direct WebSocket and verifies the origin identity.
The Pi then signs the v3 domain-separated transcript over length-prefixed
profile, environment, target identity, signer identity, private name,
certificate bytes, and private-key bytes. A `private-origin` private name is
either empty or exactly one canonical label below `mesh.shaulavo.dev`; the Pi
sets it only after the corresponding A-record reconciliation succeeds. A
`public-edge` bundle must carry an empty private name. The origin bounds all
fields before cryptographic work, verifies both identity pins, checks the key
and `*.mesh.shaulavo.dev` SAN, and rejects a bundle with a strictly earlier
expiry. An installer accepts only its configured profile, so a `public-edge`
bundle cannot enter a private-origin slot. Legacy v1 and v2 transcripts are not
accepted. An exact replay is a no-op. A different certificate with the same
expiry remains valid for emergency key rotation.

Staging bundles install into a persisted non-serving slot and never publish a
private name. Live bundles install atomically and hot-swap the TLS certificate
without restarting the daemon. A nonempty live private name is persisted beside
that slot; an empty later install preserves the last DNS-proven name. A
different nonempty name is rejected to prevent replay from renaming an adopted
identity. Intentional renames require an explicit state reset and re-adoption.
The daemon exposes the persisted name only after a valid live certificate was
installed or restored and Tailscale Serve successfully configured tailnet port
443. An expired persisted certificate triggers a new order. If renewal fails
while the current certificate remains valid, reconciliation reports the failure
and still redistributes that usable bundle to origins that were previously
offline.

Certificate distribution uses the additive `certificate.install` and
`certificate.installed` controls. The request carries the signed profile,
environment, and private name. The acknowledgement returns the profile,
environment, private name, and fingerprint. The default fan-out is four
concurrent origins with a 10-second per-origin deadline. Every successful
reconciliation redistributes the current bundle, even when no renewal occurred.

## HTTPS listener choice

The viable listener arrangements were:

| Arrangement | Result |
|---|---|
| Add TLS to T11's HTTP/WebSocket listener | Rejected. It would change the established direct terminal/bootstrap protocol and mix terminal and service exposure. |
| Bind the unprivileged daemon directly on a high tailnet port | Rejected. Every private URL would need an explicit port. |
| Grant the daemon a privileged tailnet bind on 443 | Rejected. It expands service privileges and complicates Linux and macOS packaging. |
| Bind service-only TLS on `127.0.0.1:8443`, then use Tailscale raw TCP/443 | Selected. The daemon stays unprivileged, URLs use standard HTTPS, and terminal traffic remains direct on its existing control port. |

The HTTPS listener receives only the T11 service registry. It hard-returns 404
for the configured terminal WebSocket path before dispatching to any service
handler. It never exposes terminal transport. Shutdown is bounded; a stuck
service request cannot prevent daemon exit.

`--tailscale-serve` operationalizes the selected forwarding layer after the
local Unix, control, and HTTPS listeners are ready. On every daemon start it
runs this bounded, idempotent command:

```bash
tailscale serve --bg --yes --tcp=443 tcp://127.0.0.1:8443
```

Raw `--tcp` preserves Mesh's TLS. Do not use `--tls-terminated-tcp`. Tailscale
1.52 or newer is required for this syntax. `--bg` persists the route across a
reboot and a Tailscale restart. A failed or timed-out command fails daemon
startup with the command diagnostic instead of leaving a published DNS name
without a route. The direct Mesh control listener must use a nonzero port other
than 443; the production value is 7337.

Serve-mode startup bounds Tailscale discovery to 15 seconds. Every discovered
control address must be in Tailscale's `100.64.0.0/10` IPv4 range or
`fd7a:115c:a1e0::/48` IPv6 range, at least one IPv4 address must exist, and
every discovered address must bind before the forwarding command runs. The
daemon polls the normalized address set every 30 seconds with the same
per-observation bound. A discovery error, empty result, or unusable result is
reported while the existing listeners remain active. A successfully discovered
address-set change causes an orderly listener shutdown and returns the nonzero
`daemon: Tailscale addresses changed` error. The installed supervisor then
restarts the coordinator and binds the new set. Detached workers are separate
processes and are not signaled or reaped by this restart.

## Pi configuration

Create one Cloudflare API token with DNS Write permission restricted to the
`shaulavo.dev` zone. Store it only on the Pi as a regular 0600 file. The file
contains the token and one optional trailing newline, with no other whitespace.

For the `shaul` service user:

```bash
install -d -m 0700 /home/shaul/.config/mesh
install -m 0600 /dev/null /home/shaul/.config/mesh/cloudflare.token
read -rsp 'Cloudflare API token: ' mesh_cloudflare_token
printf '\n'
printf '%s\n' "$mesh_cloudflare_token" > /home/shaul/.config/mesh/cloudflare.token
unset mesh_cloudflare_token
chmod 0600 /home/shaul/.config/mesh/cloudflare.token
```

Create `/home/shaul/.config/mesh/private-names-live.json` with mode 0600:

```json
{
  "zoneId": "<CLOUDFLARE_ZONE_ID>",
  "tokenFile": "/home/shaul/.config/mesh/cloudflare.token",
  "acmeEmail": "<ACME_ACCOUNT_EMAIL>",
  "directoryUrl": "https://acme-v02.api.letsencrypt.org/directory",
  "acceptTerms": true,
  "interval": "12h",
  "origins": [
    {
      "name": "pc",
      "tailscaleName": "pc.<TAILNET>.ts.net",
      "identity": "<PC_MESH_IDENTITY>",
      "controlPort": 7337,
      "websocketPath": "/mesh"
    },
    {
      "name": "pi",
      "tailscaleName": "pi.<TAILNET>.ts.net",
      "identity": "<PI_MESH_IDENTITY>",
      "controlPort": 7337,
      "websocketPath": "/mesh"
    }
  ]
}
```

The two accepted `directoryUrl` values are the exact Let's Encrypt production
and staging directory URLs. The runtime derives the state environment from that
URL. Unknown JSON fields, escaped or query-bearing WebSocket paths, relative
token paths, duplicate identities, and intervals outside 5 minutes through 7
days are rejected before a network mutation.

The `identity` values are the `meshIdentity` values pinned by `mesh add`; they
are not hostnames. `controlPort` and `websocketPath` must match each origin's
direct daemon listener. Include the Pi itself when it also serves private
services.

## Origin and Pi daemon setup

On Linux, run this one-time command on every origin, including the Pi. It
authorizes the unprivileged service user to update Tailscale Serve state:

```bash
sudo tailscale set --operator=shaul
```

On macOS, do not apply the Linux operator setting. Install the Tailscale CLI in
the launchd user's `PATH`, sign in through the Tailscale app, and confirm that
`tailscale serve status` succeeds as that user before enabling Mesh.

Ordinary origins use these exact daemon options:

```bash
mesh daemon --tailnet-port=7337 --websocket-path=/mesh --https-port=8443 --certificate-renewer-id=<PI_MESH_IDENTITY> --tailscale-serve
```

The Pi uses the same origin options and adds its unattended configuration:

```bash
mesh daemon --tailnet-port=7337 --websocket-path=/mesh --https-port=8443 --certificate-renewer-id=<PI_MESH_IDENTITY> --tailscale-serve --private-names-config=/home/shaul/.config/mesh/private-names-live.json
```

For the installed systemd user service, preserve T08's unit and add a drop-in
with `systemctl --user edit mesh.service`. Clear the original `ExecStart` before
setting the appropriate command above:

```ini
[Service]
ExecStart=
ExecStart=%h/.local/bin/mesh daemon --tailnet-port=7337 --websocket-path=/mesh --https-port=8443 --certificate-renewer-id=<PI_MESH_IDENTITY> --tailscale-serve
```

Add `--private-names-config=/home/shaul/.config/mesh/private-names-live.json` to
the Pi's `ExecStart`. Then run:

```bash
systemctl --user daemon-reload
systemctl --user restart mesh.service
systemctl --user status mesh.service
tailscale serve status
```

The T10 systemd unit retains `Restart=on-failure`, so the nonzero address-change
exit above causes an automatic rebind. On macOS, replace the third
`ProgramArguments` string in
`~/Library/LaunchAgents/dev.shaulavo.mesh.plist` with the origin command below.
Use an absolute home path. Add the private-names option on the Pi.

```xml
<string>exec &quot;/Users/&lt;USER&gt;/.local/bin/mesh&quot; daemon --tailnet-port=7337 --websocket-path=/mesh --https-port=8443 --certificate-renewer-id=&lt;PI_MESH_IDENTITY&gt; --tailscale-serve &gt;&gt;&quot;/Users/&lt;USER&gt;/.local/state/mesh/daemon.stdout.log&quot; 2&gt;&gt;&quot;/Users/&lt;USER&gt;/.local/state/mesh/daemon.stderr.log&quot;</string>
```

Reload the existing launchd agent in the domain where T10 installed it:

```bash
mesh_launchd_domain=gui/$(id -u)
if ! launchctl print "$mesh_launchd_domain/dev.shaulavo.mesh" >/dev/null 2>&1; then
  mesh_launchd_domain=user/$(id -u)
fi
launchctl bootout "$mesh_launchd_domain/dev.shaulavo.mesh"
launchctl bootstrap "$mesh_launchd_domain" "$HOME/Library/LaunchAgents/dev.shaulavo.mesh.plist"
launchctl kickstart -k "$mesh_launchd_domain/dev.shaulavo.mesh"
```

The T10 plist retains `KeepAlive=true`. Both installed supervisors therefore
restart the coordinator after the deliberate nonzero address-change exit. A
later installer run can replace a locally edited plist, so reapply the T12
arguments after upgrading until packaging owns these deployment-specific
options.

Startup is convergent. The Pi reconciles immediately, then every 12 hours after
a successful pass. A failed pass retries after 30 seconds with exponential
backoff, capped at the smaller of 15 minutes and the configured healthy
interval. A whole DNS, issuance, and distribution pass has a 25-minute deadline,
so one stuck provider or CA call cannot stop later passes forever.

## Staging and live acceptance commands

The one-shot command can run while the Pi daemon is active; the per-environment
renewal lock serializes them. `--staging` overrides the config's directory URL
with the exact staging URL and uses only staging state and origin slots:

```bash
mesh private-names reconcile --config /home/shaul/.config/mesh/private-names-live.json --staging --force --accept-tos
```

Confirm every configured origin received staging state under
`~/.local/state/mesh/private-tls/staging/`. A staging bundle must not change the
certificate returned by the HTTPS listener, which reads only
`private-tls/live/`.

After staging succeeds, issue and distribute production:

```bash
mesh private-names reconcile --config /home/shaul/.config/mesh/private-names-live.json --live --force --accept-tos
```

From a tailnet device, verify the standard private URL and certificate:

```bash
curl -v https://pc.mesh.shaulavo.dev/blog
```

Verify the A record is unproxied, carries comment `mesh:private-origin`, and
matches the host's current Tailscale IPv4 address. From outside the tailnet,
confirm the same public DNS answer is visible and that a TCP connection times
out. After a real Tailscale address change, do not run a one-shot reconciliation.
Confirm the origin reports `daemon: Tailscale addresses changed`, the systemd
restart count or launchd process ID changes, and the restarted control listener
accepts the new address. The Pi's manager automatically reconciles the
Mesh-commented A record and distribution target on its immediate startup pass
when the Pi changes, or on the next scheduled pass when another origin changes.
Confirm that no unmanaged DNS record changed.

## Security tradeoff

The chosen public-DNS design reveals each configured host's tailnet address to
any DNS client. The address is not a credential and remains unroutable outside
the tailnet, but it is still topology information. Split DNS would conceal it
at the cost of client-specific resolver configuration. The direct, zero-client-
configuration design is the deliberate choice here.

Every origin receives the same private key for `*.mesh.shaulavo.dev`.
Compromise of any one origin can therefore impersonate every private hostname
until the wildcard certificate is revoked and rotated. This blast radius is
accepted to keep one ACME order and renewal path while ensuring the Cloudflare
token remains only on the Pi.

## Dependencies

`golang.org/x/crypto` was already pinned for T08. T12 uses its maintained ACME
order client because the standard library has no ACME client. Cloudflare uses
`net/http` directly because the required record surface is small.

The operational path necessarily extends the existing CLI, protocol, daemon,
and Tailscale discovery packages: the CLI starts reconciliation, the protocol
delivers signed bundles, the daemon installs and serves them, and bounded
Tailscale discovery supplies DNS and listener addresses. These are integration
changes around the `internal/dnsname/` core, not alternative ownership of T13 or
T14 behavior.

`golang.org/x/sys/unix` provides atomic no-replace socket publication on the
two release platforms: `renameat2(RENAME_NOREPLACE)` on Linux and
`renamex_np(RENAME_EXCL)` on macOS. This closes a daemon socket ownership race
without relying on hard-linked Unix sockets or overwriting a replacement path.

Provider behavior follows the
[Cloudflare records API](https://developers.cloudflare.com/api/resources/dns/subresources/records/)
and its [record attribute availability](https://developers.cloudflare.com/dns/manage-dns-records/reference/record-attributes/).
The challenge lifecycle follows the
[Let's Encrypt DNS-01 guidance](https://letsencrypt.org/docs/challenge-types/)
and the [Go ACME order API](https://pkg.go.dev/golang.org/x/crypto/acme).

## Verification

Unit tests cover Cloudflare REST payloads and comments, ownership collisions,
token redaction, malformed and repeated provider responses, authoritative
propagation, cleanup, current ACME order calls, bounded ACME responses, expired
renewal, issuance deadlines, cross-process locking, descriptor-safe 0600 stores,
live and staging separation, signed profile-v3 distribution and private-name
lifecycle, identity,
environment, and profile tampering, cross-profile rejection, rollback,
concurrency bounds, manager retry scheduling, partial
discovery, bounded Tailscale output, hot TLS reload, listener shutdown,
address-change restart/rebind, and exact Tailscale Serve command ordering and
failure behavior.

`TestPrivateNamesStagingComposesACMECloudflareAndWebSocketDistribution` is the
credential-free composed acceptance test. It runs the real manager, issuer,
Cloudflare REST adapter against a fake API, authoritative propagation observer,
modern ACME order flow, real bounded distributor, `host.info` identity pin, and
signed staging install through a real daemon WebSocket. It checks A-record
reconciliation, exact TXT cleanup, staging persistence, and non-serving
isolation.

`integration/private_tls_distribution.sh` starts the compiled daemon, creates a
real T11 static service, installs identity-signed staging and live bundles over
the daemon protocol, verifies staging remains non-serving, verifies live HTTPS
hot-rotation, and confirms the terminal WebSocket path returns service-only 404.

The development machine has no Cloudflare token, no `tailscale` or `dig`
executable, and no configured Mesh origin address book. A real Cloudflare
mutation, Let's Encrypt staging order, tailnet route, outside-tailnet timeout,
and multi-host distribution were therefore not executed here. Run the staging
and live commands above on the Pi before production rollout; do not treat the
credential-free composed test as proof of the real zone or account.

The repository gate is:

```bash
gofmt -w $(git ls-files '*.go')
go generate ./...
go mod tidy -diff
go vet ./...
go test -race ./...
./scripts/verify.sh
```

T12 depended on T11 for the durable service registry and separate service
handler, and on T06 for stable Ed25519 host identities. Both dependencies were
landed before this implementation. T13 reuses the exported Cloudflare, DNS-01,
ACME, certificate, and distribution boundaries; T14 needs only the service
controls already landed in T11.
