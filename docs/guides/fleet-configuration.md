# Deliver feed and domain settings

Mesh can deliver explicitly approved dashboard feed and domain settings to selected
members of a saved fleet. This permission is separate from automatic binary
updates and update-administrator admission. Delivery does not restart a daemon,
install a release, publish a feed, or contact a provider account.

## Allow delivery on each recipient

The recipient account must already have a Mesh identity. Run this command
locally on that recipient. Use the exact coordinator identity, not its name.

```sh
mesh update config allow COORDINATOR_ID --usage-feed --expected-revision 0
```

The result contains the new target-local grant revision. Allowance replaces the
grant using that revision. It does not add an update administrator. The
coordinator must already have update-administrator admission on the recipient.
No binary policy needs to be enabled.

The grant selects usage-feed delivery, domain-policy delivery, or both. Add
`--domain-policy` to allow the primary domain and complete alias list. Each
allowance replaces the complete selected groups.

Only those fields are writable. Delivered URLs must use HTTPS and an
private service hostname accepted by the recipient's active or explicitly
proposed deployment policy. Credentials, queries, fragments, custom ports, arbitrary keys, file
paths, and headers are refused. An explicit empty string disables the feed.
A domain change retains every accepted private domain and the existing
`legacyCertificateDomain`. It cannot change services, private names, roots,
credentials, tokens, or other configuration keys.

## Preview and approve exact values

Create a selection file on the machine running the CLI. Its targets are exact
identities in the coordinator's saved `fleet.json`.

```json
{
  "targets": ["RECIPIENT_ID"],
  "usageFeed": {"url": "https://feed.PRIVATE_DOMAIN/usage.json"}
}
```

Obtain and inspect a preview. The input file is local to this CLI, never a path
on a recipient.

```sh
mesh update config preview --coordinator COORDINATOR --file selection.json --json > preview.json
mesh update config apply --coordinator COORDINATOR --file preview.json --yes --command-id COMMAND_ID --json
```

`COMMAND_ID` is a fresh 32-character hexadecimal identifier. Keep it with the
preview if a reply is lost. Repeat the exact approval with the same identifier
to recover that reply. Omitting the flag generates an identifier for that
invocation.

Preview contacts each selected recipient without waking it. Every recipient
must be reachable and have an explicit grant. The returned proposal binds the
verified approver, coordinator, complete saved membership, selected identities,
grant revisions, current selected values, and desired URL. Approval accepts
that exact proposal. It cannot silently refresh an outdated expectation or
approve another administrator's preview.

One configuration change is current on each coordinator. A new approval can
replace it only after its issued outcomes have settled. Terminal history remains
available by change identity. Changing a fleet endpoint, revision, dependency,
platform, or member blocks further dispatch. Restoring the old file alone does
not clear that block.

## Read status and recover a change

```sh
mesh update config status CHANGE_ID --coordinator COORDINATOR --json
mesh update config retry CHANGE_ID --coordinator COORDINATOR --expected-revision N --command-id COMMAND_ID --json
mesh update config cancel CHANGE_ID --coordinator COORDINATOR --expected-revision N --command-id COMMAND_ID --json
```

Status reports each target's publication state, issued uncertainty, component
receipt, and actual dashboard observations separately. `saved` means the
selected field and its durable receipt were published. A selected local edit
causes a conflict. Unrelated supported settings, including host records and the
dashboard theme, survive the merge. Malformed or unknown fields cause an error;
delivery does not repair or normalize them.

Only the currently trusted approving actor may retry or cancel its change.
Retry keeps the exact proposal and expectations. A local edit needs a fresh
preview and approval after the prior change settles. Owner withdrawal creates a
sticky block. Restored admission needs an explicit retry. Configuration
ownership does not follow binary-policy ownership or fleet-note authorship.

On the coordinator itself, its existing local identity may use
`cancel ... --retire --expected-revision N` to stop and archive a withdrawn
owner's change after issued work settles. This does not take ownership or resume
its delivery. A different approver then creates a fresh preview and approval.
If retirement first stops pending work while issued work remains, inspect its
settlement and invoke retirement again with the resulting revision.

Cancellation prevents new dispatch intentions. Calls already reserved may still
arrive and save their selected field. They remain issued or uncertain until
observed. Cancellation does not undo saved bytes. Withdraw target-local
acceptance separately on the recipient:

```sh
mesh update config revoke --expected-revision N
```

Revocation serializes with recipient acceptance. A later call carrying the old
grant revision is refused. An earlier accepted publication may already exist.
Status preserves that distinction. Neither configuration revocation nor binary
policy disable changes the other permission.

## Check the running dashboard

An already running dashboard watches its local configuration once per second.
Changing a valid URL cancels and joins the prior fetcher before starting its
replacement. Disable and invalid configuration clear current quota values.
Recovery can enable a dashboard that started with the feed disabled. A new
feed's revision 1 cannot be mistaken for the prior feed's revision 1.

The renderer acknowledges the generation and publication it applied in its real
clock frame. Only then may the joined watcher report it loaded. It renews that
state every 10 seconds against a 30-second lease. A daemon restart requires a
fresh actual renewal. The observation uses the existing local identity and Unix
socket. It creates neither an identity nor a daemon. Remote callers cannot
submit dashboard success.

No active observer means `saved; no active dashboard observation`, not complete.
A live observer with a different digest means reload pending. A matching loaded
configuration can still report a failed feed read. Status polls use bounded
calls and may take up to 30 seconds to reflect a new consumer observation.

Heartbeat renewal is process liveness, not a new HTTP request or quota reading.
Existing fetch limits remain unchanged: at least one minute between subsequent
fetches, one request in flight, a 10-second default timeout, a 15-second maximum,
a 64 KiB response cap, normal TLS validation, and no redirects. Publication and
upstream observation times remain original. Unknown quota stays null.

Mesh's writer lock coordinates supported local edits. Quiesce external editors
that ignore it during delivery. The configuration writer cannot provide a
portable filesystem compare-and-swap against an editor that bypasses its lock.


## Deliver domains and inspect actual consumers

Select the primary domain and the complete alias list. Keep every currently
accepted domain, moving the old primary into aliases when necessary.

```json
{
  "targets": ["RECIPIENT_ID"],
  "domain": {"primary": "NEW_PRIVATE_DOMAIN", "aliases": ["OLD_PRIVATE_DOMAIN"]}
}
```

You can select `domain`, `usageFeed`, or both in one proposal. Each component
has its own expectation, revision and durable receipt. One saved component
survives a conflict in the other component. Delivery does not roll back a saved
component over a later local edit.

Saving a domain policy does not reload a process. `domainConsumerState` reports
`daemon restart required` until the actual signed daemon observation matches
its saved digest. That observation includes a fresh process instance identity.
Use the host's existing native service manager during approved maintenance.
Configuration delivery does not restart the daemon or any session. Reopen old
dashboards to load their immutable domain policy too.

A policy generation fences all remote service-cache commits and retained live
service and private-name observations. Old-policy processes cannot republish
invalidated snapshots as current, even after a crash between domain publication
and cache invalidation. Only affected remote snapshots are deleted. Local
services, sessions, routes and certificate slots remain intact. A failed or
incomplete advisory cache publication stays unavailable until a complete fresh
observation replaces it. SQLite contention remains a skipped advisory write,
not a failed live host observation.

Actual service HTTPS verification is unknown. Mesh does not send verification
requests because the current HTTP admission contract can start an on-demand
service after a previously running or non-demand snapshot. Catalog health and
feed reads remain separate observations. Neither proves an application-specific
HTTPS check.
