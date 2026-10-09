# Private temporary apps with short URLs

Status: Approved. Temporary-app public sharing and widget rendering are removed.

Implementation and checks: [T29](../tasks/T29-temporary-apps.md).

## Outcome

An agent or person creates a disposable website on a Mesh origin and receives a
short URL such as `https://7k3d.new.example`. The website contains static files or
runs an explicit HTTP server recipe. Every temporary app is private. Its owner
can inspect, update, renew, download or delete it.

Keep an app running while admitted traffic extends its deadline. After 24 hours
without activity, withdraw access, stop its processes and clean up its managed
files. Keep the reusable floating widget source dormant; Mesh does not render
it on apps or serve its control routes.

## Execution contract

| Concern | Behavior |
|---|---|
| Address | Four-character ID under the configured deployment domain, with private Tailnet DNS and HTTPS ingress. |
| Reservation | App IDs and hostnames remain reserved after expiry or deletion. Ordinary service or tunnel updates cannot erase an app. |
| Access | Verified Tailnet connection and HTTPS precede browser pairing, owner checks, app content and upgrades. |
| Owner | The stable Mesh identity of the origin. Another enrolled host is not automatically its owner. |
| Activity | Admitted HTTP requests, streamed data, WebSocket application frames, successful updates and explicit renewal extend the deadline. |
| Exclusions | Denied traffic, management polling, idle upgrades, health checks and WebSocket ping/pong do not extend it. |
| Renewal | Set the deadline to 24 hours from now. Expired or deleted apps cannot be revived. |
| Cleanup | Remove access before stopping workers and deleting managed source and local app data. Original source remains untouched. |
| Upgrade | Preserve existing owner files and app identities. Ignore obsolete sharing metadata when deciding access. |

## Ownership and request flow

The origin owns uploaded source, setup recipes, labelled workers, verified
loopback listeners and workspace cleanup. An independent private app registry
owns name allocation, browser grants, traffic deadlines and signed lifecycle
acknowledgements. The registry is separate from ordinary service snapshots, so
replacing a service snapshot cannot erase an app or restore a sharing policy.

Owner operations enter the origin through its local Unix socket or authenticated,
exact-host-key-pinned Mesh SSH. The origin signs requests to its configured
registry. Direct Tailnet owner RPCs cannot ask it to sign arbitrary operations.
Terminal traffic remains direct between hosts.

HTTP admission first requires HTTPS and a verified Tailnet device address.
Trusted private forwarding preserves that address; HTTP forwarding headers
alone never establish it. Browser grants do not override this ingress boundary.
Only owner-authorized app requests reach the origin, with a signed, bounded,
one-use admission proof. The origin validates app identity, generation, lease,
method and URI before forwarding to its verified loopback server or static root.

Management credentials stay on the management host. App view cookies are
app-scoped, and management credentials are stripped from upstream requests.
App admission rejects foreign Origin and cross-site browser requests. WebSocket
upgrades require the app's exact Origin. Responses restrict cross-origin reads
and framing without weakening stricter app policy. Apps keep GET read-only and
protect mutations from legacy clients that omit browser headers.

Browser pairing starts with a same-origin POST. Pending codes remain bounded,
in-memory and expire; approved grants remain durable. CLI approval shows the
browser's source hints and asks for confirmation. Revocation removes the grant
and associated view authority. Management pages remain separate from app code.

## Lifecycle and recovery

Copy selected source into a managed workspace. Validate archive paths, links,
credential-shaped files, source limits, storage mounts and available space before
setup. Run installation only through an explicit setup recipe. Server commands
must bind loopback, and process ownership checks must prove the listener belongs
to the app. Ordinary service registration cannot expose managed app roots or
app-owned ports, including dormant service recipes.

Stage updates beside the working source. Preserve app ID and URL; renew the
inactivity deadline after activation. Persist create receipts, update candidates
and activation intent so interrupted operations recover without duplicate apps
or mixed workspaces. Failed updates retain the previous ready source, and owner
inspection reports bounded setup output and listener failures.

Remove access and cancel admitted streams before expiry cleanup. Short registry
leases prevent an origin from continuing to serve through a partition. Preserve
source during unavailable or lost registry state according to the existing lease
and cleanup grace period. Downloads use bounded source snapshots and atomic
publication without overwriting the caller's destination.

## Retained widget

`web/app-pill` contains the Solid floating control and React Grab adaptations.
`internal/apppill` contains its generated assets, HTML transformer and fixtures.
Keep their provenance, drag/flick behavior, keyboard controls, placement and
reduced-motion coverage available for the owner's other project. No active
app response imports the transformer, injects the pill or exposes its assets.
Widget source details live in [its README](../../web/app-pill/README.md).

Public/private commands, visibility fields, sharing endpoints and visitor paths
are removed. Historical sharing state must not restore them after restart.
Ordinary explicitly named public services and SSH tunnels retain their existing
registration and access rules.

## Verification

Exercise static and server creation, updates, source download, renewal, deletion,
expiry, setup failures and daemon restart. Prove existing source survives upgrade.
Probe old sharing operations, public app URLs, management and widget routes,
WebSocket upgrades and restored obsolete state. All rejected requests must fail
before origin resolution or content forwarding.

Verify HTTPS and Tailnet admission independently from browser cookies. Check
foreign origins, revoked grants, exact-origin upgrades, unauthorized source
downloads and loopback process ownership. Confirm an ordinary named public
service and a named SSH tunnel still work.

Run app, CLI, registry and preserved-widget checks followed by the repository's
race, vet and integration gates. The widget fixtures remain source-reuse tests;
they do not authorize reconnecting it to Mesh app responses.
