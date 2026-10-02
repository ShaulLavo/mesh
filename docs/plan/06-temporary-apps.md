# Temporary apps with short URLs

Status: Approved, 2026-09-30. Implemented for PR review; production rollout pending.

Implementation and checks: [T29](../tasks/T29-temporary-apps.md).

## Outcome

An agent or a person creates a disposable website on a Mesh host and gets a URL
such as `https://7k3d.shaulavo.dev`. The website can contain static files or run
an HTTP server in any installed language. Each app starts private. Its owner
can make it public and private again without changing the URL.

Everyone who can view the app gets a floating control. It starts as a small dot,
expands into a pill, and supports dragging and flicking to a screen edge.
Public visitors can copy the link. A recognized owner can also change
visibility, renew the app, download its source, and delete it.

Keep the app running while it receives traffic. After 24 hours without activity,
expire access, stop its processes, and delete its managed files. Do not add a
fixed maximum age or a separate idle-sleep policy in this work.

## Settled behavior

| Concern | Execution contract |
|---|---|
| Address | Four lowercase Crockford base32 characters directly under `shaulavo.dev`; no category prefix or path prefix. |
| Reservation | Reserve names atomically across apps, services, and tunnel claims. Retry random collisions. Retain a small used-name record after deletion. |
| Initial visibility | Owner-only, enforced at the edge before forwarding any app content. |
| Public visibility | Anyone can use the app without a Mesh account. Only the owner can manage it. |
| Owner | The stable Mesh identity of the origin host that owns the app. Pair a browser explicitly with that identity. Other hosts are not owners merely because the edge trusts them. |
| Idle expiry | `last_activity + 24h`, initially measured from creation. Activity keeps extending the deadline. |
| Activity | Admitted app HTTP requests, application data on WebSockets, streamed app data, successful owner updates, and explicit owner renewal. |
| Excluded activity | Rejected access, Mesh health checks, pill status polling, connection establishment without application traffic, and WebSocket ping/pong frames. |
| Public bots | Their admitted app traffic counts. Do not attempt to distinguish a human from a bot. |
| Visibility change | Keeps the URL and existing deadline. Making private closes non-owner streams and blocks subsequent non-owner requests. |
| Renewal | Reset the deadline to 24 hours from now. Never revive an expired or deleted app. |
| Expiry or manual deletion | Remove access first, then stop the app and delete its workspace, app-specific build files, local data, and retained app output. |
| Expired URL | Show a generic expired-app page. Never assign the name to an unrelated app. |

These controls concern Mesh hosting. An app may implement its own application
login independently. Making a public app private cannot retract bytes already
delivered to a visitor.

Private-app owner authority from Tailnet recognition or a view cookie applies
only to same-origin requests, direct browser requests, and top-level GET or HEAD
navigations. A foreign Origin always receives 403, even without Fetch Metadata.
Other same-site and cross-site browser requests receive 403 before resolving or
forwarding to the origin host. Without Fetch Metadata, every method requires no
Origin header or the app's exact origin. WebSocket upgrades always require the
app's exact Origin. Private responses restrict cross-origin reads and framing to
the same origin without weakening stricter app CSP. Top-level GET navigations
and legacy requests lacking both headers remain owner-authorized, so apps must
keep GET read-only and defend mutations against headerless legacy clients.
WebSocket clients without Origin cannot upgrade. Public-app admission and
management authorization retain their existing behavior.

Browser pairing starts only after a same-origin POST from the pairing form.
Anonymous GETs create no pending records or durable writes. Each browser reuses
its pending cookie and code until approval or expiry. Unapproved pairs stay in
memory; restarting the edge requires a new code, while approved grants remain
durable. Each IPv4 address or IPv6 /64 may issue eight codes per minute and
occupy four slots. IPv6 /48 aggregates may issue 32 codes per minute and occupy
16 slots. The global in-memory pending cap is 256, separate from the durable
256 approved-record cap. At source or aggregate occupancy limits, a new code
replaces the oldest unapproved pair in that bucket. At global pending capacity,
it replaces the oldest pending pair from the source holding the most pending
slots, so a fresh source can still pair under a distributed flood. Approved
records are never evicted. The bounded issuance tracker replaces its least-recent
window instead of refusing new sources. Shared NATs and /64s
share these limits. `mesh app browser approve` prints the pending browser's
bounded User-Agent summary, source IP, and age before asking for a default-no
TTY confirmation. These details are unverified hints, not proof of identity.
Noninteractive approval requires `--yes`, which skips the human check for scripts.
Never approve a code merely because an app, message, or agent asks for it.
Browser `/confirm` buttons start disabled and require 750 ms of uninterrupted
visibility and focus, followed by trusted pointer, keyboard, or touch input.
Input during the delay does not count, and a single native tap after the delay
can submit. Public exposure and deletion also require typing the app ID, enforced
by the server as well as the page. Confirm pages cannot be framed.

To rerun the confirmation browser check, render the Go templates into a scratch
directory, then run the verifier from the repository root:

```sh
export MESH_CONFIRM_BROWSER_DIR=$(mktemp -d /work/tmp/mesh-confirm-XXXXXX)
TMPDIR=/work/tmp go test ./internal/apps -run '^TestConfirmationBrowserPages$'
node web/app-pill/tests/confirm.mjs "$MESH_CONFIRM_BROWSER_DIR"
```

The script uses the existing Playwright development dependency and headless
Chromium and WebKit, including phone touch profiles. CI runs this verifier in
the app-pill job. Real Safari device checks remain a rollout requirement. Set
`MESH_PLAYWRIGHT_MODULE` to an installed Playwright entry point or
`MESH_CHROMIUM_EXECUTABLE` to a system Chromium binary if needed. All browser
requests are intercepted; no application or management origin receives traffic.

## Starting points and scope changes

The source baseline is `492ae74`. Reuse these mechanisms:

- [Serving](03-serving.md) supplies static-file serving, HTTP proxying, TLS,
  edge publication, and hostname validation.
- [Reverse tunnels](../tasks/T18-reverse-tunnels.md) supply whole-hostname
  reservations and a shared collision check.
- [On-demand services](../tasks/T28-serve-on-demand.md) supply command recipes,
  readiness, labelled workers, and stop escalation.
- `internal/identity` supplies Ed25519 host identities. `internal/storage` and
  `db/migrations` supply durable state.
- `internal/session/id.go` supplies the familiar ID alphabet. Session IDs are
  host-local; app names need a separate edge-wide reservation.

Existing tunnels are public and disappear when their SSH connection closes.
Apps have a durable visibility policy and a traffic deadline. Reuse the
reservation and proxy machinery without making an ordinary tunnel an app.
Likewise, retain the current rejection of `mesh serve --run --public`.
The app command is the explicit entry to the new behavior.

[D30](01-decisions.md#d30--temporary-apps-have-an-owned-lifecycle) records the
approved exception to the old exclusion of previews and share links in D22.
Ordinary `serve` and `ssh -R` keep their explicit-name rules. Allocating an app
name does not itself grant public access.

## Command contract

The `mesh app` family uses the owner host first and its configured public edge:

```sh
mesh app create pc ./site
mesh app create pc ./app --run 'bun run start --host 127.0.0.1' --port 3000
mesh app list pc --json
mesh app inspect pc 7k3d --json
mesh app update pc 7k3d ./app
mesh app public pc 7k3d
mesh app private pc 7k3d
mesh app renew pc 7k3d
mesh app download pc 7k3d ./saved-app.tar.gz
mesh app delete pc 7k3d
mesh app browser approve pc CODE
mesh app browser list pc
mesh app browser revoke pc BROWSER
```

Creation copies the supplied directory into a managed workspace on the target
host. Resolve relative input paths on the caller. Print the app URL, owner host
and fingerprint, visibility, deadline, and runtime state. Return those values
in JSON for agents. Creation always starts private; public sharing is a
separate explicit action naming the app.

Public failure pages must not expose commands, environment values, workspace
paths, or output tails. Keep those diagnostics available through owner controls
and the authenticated CLI.

Require explicit command and port for a server. Do not infer a framework or
install dependencies automatically. Allow an explicit setup command in the
managed workspace for apps that need dependencies. Keep setup progress and
failures inspectable. Support any installed runtime that serves HTTP.

Use an idempotent request ID for create and update. Stage uploads before
activation. A dropped connection must not create duplicate apps or replace a
working app with a partial upload. Exclude `.git` and known credential files by
default; refuse unsafe source selection with an actionable error. Validate
archive paths, symlinks, entry counts, and byte limits before writing files.

After an owner-signed update, preserve the app identity and visibility. Reset
its activity deadline. Report startup failure explicitly. Do not claim seamless
deployment or add a version-selection product to this work.

## Responsibilities and durable state

Put app domain types and lifecycle operations in a new `internal/apps` package.
Put browser pairing and session authorization in a new `internal/webauth`
package. Add app CLI handling under `internal/cli`. Adapt the existing daemon,
edge, storage, and protocol packages at their boundaries.

The origin owns the process and workspace. The edge owns the global name,
visitor access policy, browser sessions, admitted traffic deadline, and public
route. This does not centralize terminal-session state or relay terminal traffic.

Keep visibility independent from runtime state. A private app may be running,
starting, failed, or temporarily offline. Track deletion separately so cleanup
can continue after access has already expired.

Persist the app ID, owner identity, origin identity, edge identity, creation
time, last activity, deadline, visibility, policy generation, and cleanup state.
Store the workspace, recipe, and worker reference on the origin. Retain only
the minimal used-name and replay records after cleanup; remove app payloads
and sensitive output.

Use signed, versioned operations and exact retry acknowledgements for origin
control. Reuse the outbox pattern from tunnel claims. Check the exact owner,
edge, app ID, sequence, and operation; an authorized second host cannot mutate
someone else's app. Bound durable records and transfers using existing edge
capacity rules, with separate capacity for used-name records. Refuse new apps
when capacity is exhausted without preventing cleanup.

Serialize traffic admission, visibility changes, renewal, and expiry against
the current app generation. A request at or after the deadline cannot revive
an app. Reconcile deadlines before serving requests after a restart. Persist
activity sufficiently to preserve the documented deadline across daemon crashes.
Use an injectable clock for deterministic tests.

Expiry must block the hostname even when the origin is unreachable. Queue
origin cleanup durably. On reconnect, finish cleanup before considering any
restart. Give the origin a renewable deadline so a partition cannot leave its
process running forever. Reconcile activity with authenticated edge updates.
An offline origin can retain files until it comes back; show pending cleanup
honestly. Report cleanup errors and retry instead of pretending deletion finished.

## Workspace ownership

On this Linux host, configure the app workload root as `/work/mesh/apps`.
Keep app directories, dependencies, local databases, uploads, and build output
there. Keep small Mesh metadata in its existing state directory. Verify the
configured mount and free space before accepting substantial uploads or setup
downloads. Fail if the required mount is missing.

Other hosts require a configured writable workload root suited to that host.
Preserve existing directory ownership. Never place app payloads directly in
`/work/projects`, and never recursively change a drive's ownership.

Delete only an app's recorded managed root. Use anchored filesystem operations
and reject traversal or symlink escape. Stop the owned worker before removing
files. Do not delete the caller's source directory, a shared dependency cache,
another app, or an external database. App workers run with the current Mesh
execution model; this feature does not establish OS sandbox isolation.

## Browser ownership and the floating pill

Reserve `apps.shaulavo.dev` as the management origin after checking for a
pre-existing claim or service. This longer address is for login and management;
shared app URLs remain short. Keep its host out of random app allocation.

Start pairing from that trusted page. Create a high-entropy, short-lived browser
challenge and display its approval code. The owner approves from an existing
Mesh CLI or SSH session using `mesh app browser approve`. Bind approval to that
pending browser challenge, edge, and exact owner identity. Consume it once.
Rate-limit guesses, expire pending challenges, and never embed an owner secret
in the shared app URL. Print reachable links and commands for a Mac or phone;
do not depend on a QR code displayed on this host.

Issue a revocable, host-only `Secure`, `HttpOnly` management cookie with a
`__Host-` name. A browser
may be paired with more than one origin identity through separate approvals.
Pairing with one identity grants no authority over another identity's apps.
Do not add passwords, OAuth providers, or general team roles in this version.

Every management mutation requires a valid session, an exact ownership check,
an allowed request origin, and CSRF protection. Same-site sibling subdomains
are not sufficient proof of authority. Check revocation server-side. Support
owner-signed CLI recovery if a browser is unavailable.

For private app viewing, exchange owner authentication through a top-level
redirect into a short-lived, app-scoped view session. Its cookie is host-only
and grants viewing only. The edge consumes and strips Mesh cookies before
forwarding to the app. Prevent upstream responses from overwriting reserved
Mesh cookies. Private assets, APIs, upgrades, and streams use the same gate.
Disable intermediary caching of private responses and access decisions. Making
private must also invalidate or bypass any cached public response at the front door.

Inject an unprivileged mount script into app HTML. Use Shadow DOM for the
floating shell and a separate-origin management frame for authenticated UI.
Keep owner credentials, CSRF tokens, and privileged API calls inside the
management origin. Shadow DOM is style isolation, not an authorization boundary.
Allow frame embedding only from the exact app hostname. Validate message
origins, source windows, and message shapes. Parent messages may resize and
position the pill; they must not issue management actions.

Keep the dot, dragging, expansion, and visitor controls available without login.
Owner controls become available after recognition. If embedded authentication
is unavailable in a browser, use a top-level management tab and return to the
same app. Preserve the visitor pill rather than requiring everyone to log in.

For visibility changes and deletion, confirm the operation in a trusted
top-level management view naming the exact app. An arbitrary app can overlay
an embedded control, so framed buttons alone must not authorize those actions.
Return to the app after confirmation. Test that a sibling app cannot induce
an owner mutation through messages, forged requests, or a framed confirmation.

## Reuse React Grab's Solid toolbar

The reference checkout is `/work/projects/references/react-grab`. Pin copied
source to commit `ea4bbec9e80f4802e8ae19ad18431edb9ddbb670`. Preserve its MIT
license and provenance in `third_party/react-grab-toolbar`.

Extract the drag, velocity projection, edge positioning, collapse, viewport,
and mount behavior from `packages/react-grab/src`. Replace selection controls
with Mesh controls. Exclude React inspection, page freezing, agent bridges,
and the rest of React Grab's runtime. Its collapsed handle needs adaptation
to become a dot and support dragging while collapsed.

Add the Solid UI under `web/app-pill`. Pin SolidJS, Vite, and the Solid Vite
plugin with a lockfile. SolidJS supplies the requested reactive UI; Vite and its
plugin compile that UI into self-contained assets. These are build dependencies.
Embed generated assets in the Go binary. Installed hosts need no Node runtime,
CDN dependency, or extra release payload. Preserve the single-binary installer.

Use upstream motion as the starting point. Keep drag movement immediate,
snap using release velocity, remember edge position, and support interrupted
expansion. Add keyboard access, touch input, safe-area offsets, focus handling,
and reduced motion. Use a practical touch target around the small visual dot.

## Inject without changing app semantics

Reserve `/.mesh-app/` for the pill's local assets and bootstrap. Route that
prefix at the edge and never forward it into the app. Document the reserved
prefix. Inject once into HTML documents, including error pages where suitable.
Leave JSON, downloads, binary responses, API payloads, and upgrades unchanged.

Make the response transformer handle compressed HTML, charset detection,
streamed HTML, CSP headers and CSP meta tags, content length, ETags, and cache
validators. Permit the exact Mesh script, styles, and frame without removing
the app's entire CSP. Update relevant policies for embedded UI. Do not buffer
an unbounded response or delay streamed rendering until the document finishes.
Account for service-worker navigation caches and existing COOP/COEP behavior.
Prefer identity encoding from the upstream. Use Go's standard gzip support.
If an upstream still sends Brotli, justify a pinned decoder dependency in its
implementation brief; decoding is required to transform that HTML correctly.

Prove the transformer early using plain HTML, React hydration, Solid, strict
CSP, gzip and Brotli, streamed responses, and mobile browsers. A non-HTML
endpoint has no document into which to mount a pill; its server remains usable.
Document any unsupported document encoding rather than silently corrupting it.

## Execute in this order

1. **Prove the browser mechanics.** Build a small Solid extraction of the pill
   and an HTML-injection harness. Exercise strict CSP, hydration, mobile drag,
   the management frame, and top-level owner confirmation. Deliver a working
   dot-to-pill interaction before expanding backend integration.
2. **Add app records and managed workspaces.** Implement collision-safe names,
   owner identity, durable deadlines, staged copy/upload, bounded transfer,
   static apps, server recipes, readiness, logs, and idempotent cleanup.
3. **Add browser pairing and access gates.** Implement browser approval,
   revocation, view sessions, owner checks, and private-by-default whole-host
   routing. Prove private APIs and upgrades are protected before public sharing.
4. **Connect the pill and owner operations.** Implement public/private,
   copy link, source download, renewal, deletion, and trusted confirmation.
   Preserve the short URL through visibility and source changes.
5. **Complete expiry and recovery.** Track app traffic, serialize expiry races,
   terminate non-owner streams on privacy changes, reconcile restarts and
   partitions, and remove app data. Retain only used-name and replay records.
6. **Finish distribution and live verification.** Embed UI assets, extend
   release checks and agent guidance, verify wildcard DNS/TLS and the actual
   front door, and run the feature from a remotely reachable browser URL.

Each step lands with focused verification. Expand testing when a later step
changes an earlier assumption. Update this plan and the status index as work
lands. Leave unrelated [agent interface](05-agent-interface.md) work intact.

## Acceptance and completion

- Create a static app and a frontend with a real backend on a remote host.
  Verify root-relative assets, redirects, API calls, and WebSockets.
- Before sharing, an anonymous browser receives no app content. Pair a browser
  and verify that its owner can view and manage the app at the same short URL.
- Share publicly. A separate anonymous browser gets the app and visitor pill.
  It cannot change visibility, renew, download private source, or delete through
  direct requests or edited UI. Public mode does not publish source archives.
- A second authorized Mesh identity cannot manage the first owner's app.
  Replayed approval codes and revoked browser sessions fail.
- Make private. New non-owner requests fail, existing non-owner streams close,
  and front-door caches cannot continue serving the app. Owner access remains.
- Drag and flick the expanded pill and the collapsed dot using mouse and touch.
  Verify docking, persisted position, keyboard access, and reduced motion.
- Move the clock forward. App traffic resets the deadline. Health checks,
  rejected visits, pill polling, and idle sockets do not. Verify an expiry race
  cannot restore access. Delete app code and local data while preserving the
  source directory, shared caches, and another app.
- Restart both daemons and interrupt upload, setup, runtime startup, visibility
  changes, and cleanup. Recover without duplicate names or public defaults.
  An unavailable origin leaves access expired and cleanup pending until recovery.
- Verify generated UI assets, attribution, and cross-platform release archives.
  Run `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`, and
  `scripts/verify.sh`. Add integration coverage for the app and browser flow.
  Put build scratch and configurable caches on `/work` for this host.
- Verify browser behavior through the T3 collaborative preview when available.
  Exercise Safari on a Mac or phone for pairing and embedding behavior before
  declaring that compatibility complete. Return reachable URLs and evidence.

Wildcard DNS, management-name availability, actual TLS termination, and cache
behavior have not been checked on the live edge. Resolve those through
read-only inspection before rollout. Existing source support is not evidence
that the deployed front door is configured correctly.

This planning change requires Markdown-link and diff checks. Implementation
requires the functional, security, lifecycle, browser, and release checks above.

## Source references

- [Claude Code artifacts](https://code.claude.com/docs/en/artifacts) establish
  private-by-default pages with header sharing controls.
- [React Grab toolbar](https://github.com/aidenybai/react-grab/tree/ea4bbec9e80f4802e8ae19ad18431edb9ddbb670/packages/react-grab/src/components/toolbar)
  and [MIT license](https://github.com/aidenybai/react-grab/blob/ea4bbec9e80f4802e8ae19ad18431edb9ddbb670/LICENSE)
  supply the reusable Solid UI source.
- [Browser origin isolation](https://developer.mozilla.org/en-US/docs/Web/Security/Defenses/Same-origin_policy),
  [cookie scope](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Set-Cookie),
  and [authorization checks](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html)
  support the separation of app content from owner management.
