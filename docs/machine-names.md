# Machine names

Each Mesh destination owns one machine name and a monotonically increasing
revision. The daemon creates its first declaration at startup from the local
Tailscale name or hostname. `mesh rename` changes that existing declaration.
An authenticated `host.info` reply or host state observation supplies
that declaration to a viewer. The destination's own card reads the local daemon
through its trusted socket; displaying it creates no self-adoption record.

```bash
mesh ls
mesh rename HOST_ID work-pc
mesh rename work-pc study
mesh HOST_ID
mesh update --host HOST_ID --check
```

Rename uses the destination's current revision and exact cryptographic identity.
It preserves connection addresses, sessions, worker processes, shell processes,
terminal input/output, dashboard settings, and user-managed SSH profiles.
Remote rename requires the destination's full-device grant. An update-only grant
continues to authorize signed update operations only. Owner-local rename uses
the trusted daemon socket.

Names are lowercase command-safe identifiers: letters, digits, and internal
hyphens, starting and ending with a letter or digit, up to 63 characters.
Mesh validates the length, reserved command
names, and session-ID ambiguity. The rename command prints the accepted owner
revision. Repeating the current name leaves that revision unchanged.

## Conflicts and disconnected machines

A bare name requires a fresh authenticated reply from that destination before
an effect. If the destination has renamed, the command refuses the retained
name and prints the exact ID to use. Exact IDs keep their targets across rename
and can select offline update members.

Two known owners can claim the same name. Viewers project the same received
claims deterministically: the lower stable identity has priority, and conflict
labels include identity suffixes extended until distinct. Bare-name commands
refuse such a conflict and print both exact IDs. Dashboard conflict cards also
show the full identities. Priority identifies the deterministic projection;
it grants no authority to target an ambiguous name.

Cached claims retain the last authenticated owner name. Lists, picker views,
dashboard cards, and update output show "cached name" when the displayed
name lacks a current verification. The destination still owns that declaration.
Fresh verified views show the name alone.

Name freshness and connectivity are separate. A reachable machine can have a
cached name while its name observation is missing, failing, or at least 30
seconds old. The dashboard also marks the name as cached when the last reply
is at least 30 seconds old or the connection is connecting, unreachable, or
refused. Metrics and catalog ages describe their own observations.
Older revisions and a different name at the same revision are refused.

There is no central naming authority. Viewers with different received claims
can show different retained names during a partition. A rename can refuse a
known collision, while an unseen competing claim can still cause a conflict.
Other viewers adopt a new declaration after reconnecting. A disconnected
viewer cannot establish that a name is globally unique.

## Storage boundaries

`hosts.json` holds connection records and dashboard configuration. Product
viewer aliases are no longer read or persisted; an ordinary address-book write
discards their obsolete fields. Owner declarations live in private
`machine-name.json` state, and authenticated remote claims in the private
`machine-names` cache beside the address book. Fleet and update records persist
stable membership identities and addresses; displayed names are ephemeral.

External SSH profile aliases and configured SSH connection settings remain
user-owned. Bootstrap resolves those profiles and checks both the original
and resolved destination pins before connecting. Machine-name declarations do
not rewrite those profiles, DNS names, or the operating-system hostname.
