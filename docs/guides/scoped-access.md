# Grant scoped read access

Use a dedicated Mesh key to let a remote program observe one host's identity,
session status and configured service names. The target enforces this narrow
representation. Metadata alone cannot read terminal output. Scoped keys cannot attach, execute commands,
copy files, manage services, use Mesh SSH or administer updates.

## Create a dedicated credential

On the caller's machine, choose a new private directory:

```bash
mesh device scope keygen --key-dir KEY_DIRECTORY --json
```

The result contains the public `identity`. Keep the private `identity.key` on
that machine. Keygen never overwrites an existing key. Existing malformed or
unsafe credentials fail. Loading or reading a missing credential creates nothing.

## Grant access on the target

Use the target's local account to read its current policy and grant the public
identity at that revision:

```bash
mesh device scope list --json
mesh device scope grant --identity PUBLIC_ID --permissions metadata \
  --expected-revision 0 --expires-in 24h --json
```

All policy commands run on the target's filesystem. They accept no `--host` and
contact no daemon. They require an existing target identity. A full account
controller can also invoke these commands through its existing account powers.

The default lifetime is 24 hours. The maximum is seven days. Use an absolute
RFC3339 `--expires-at` timestamp for an exact retry after an uncertain write.
`--expires-at` and `--expires-in` are mutually exclusive. Repeating a duration
later renews the grant and requires the current revision.

The key must differ from the target identity and from every registered full-device
or update-administrator key. Managed writers enforce that exclusion together.
An expired scope remains registered until explicitly revoked. Full-device and
update-administrator registration can still coexist without a scope.

## Read one metadata page

The caller needs an existing saved host record and its destination pin. Pass its
exact host ID and the explicit credential directory, including for the same host:

```bash
mesh device scope read --host HOST_ID --key-dir KEY_DIRECTORY --json
mesh device scope read --host HOST_ID --key-dir KEY_DIRECTORY \
  --section sessions --limit 100 --json
mesh device scope read --host HOST_ID --key-dir KEY_DIRECTORY \
  --section services --json
```

The default section is `host`. Session pages may use `--session SESSION_ID` for
one exact row. Row pages accept 1 to 500 entries, default 100. Pass a returned
`nextPage` as `--cursor`; it belongs to that target, credential, grant incarnation,
section and filter. Each response is a live, non-atomic observation. Concurrent
catalog changes may affect later pages. Encoded responses are at most 256 KiB.
The default timeout is five seconds, with a range from 100 ms to 60 seconds.

Host metadata contains only its ID, declared machine name and build version.
Session metadata contains IDs, state and recorded creation, attachment and exit
facts. Unknown detach time is null. Service metadata contains only name, kind,
local/tailnet scope and whether it is configured. Commands, paths, labels,
profiles, previews, logs, recovery records, service targets and runtime errors
are excluded. Listing services performs no health probe or demand start.

These commands use only direct pinned TLS with the explicit key. They never try
the ordinary key, Unix or worker sockets, SSH, bootstrap, wake, local saved output
or owner caches. No reachable configured listener means the read fails. Each
command requires `--json` and writes one agent result, including failures.

## Grant optional session output

On the target, replace the grant at its current revision:

```bash
mesh device scope grant --identity PUBLIC_ID --permissions metadata,session-output \
  --expected-revision 1 --expires-in 24h --json
```

`session-output` covers every session on this host, including other agents'
output, saved screens and diagnostic-mixed log tails. This is not creator-only
access. Replacing permissions retires old connections even when expanding access.
Reconnect with the same dedicated credential after the replacement.

On the caller:

```bash
mesh device scope output --host HOST_ID --key-dir KEY_DIRECTORY \
  --session SESSION_ID --format tail --max-bytes 65536 --json
mesh device scope output --host HOST_ID --key-dir KEY_DIRECTORY \
  --session SESSION_ID --format preview --cols 80 --rows 24 --json
```

Tail defaults to 64 KiB and permits at most 1 MiB. `bytesBase64` preserves binary
bytes. Source is `live-tail` for the worker ring or `saved-tail` for the durable
log. These are bounded tails, without exact stdout/stderr, cursor or EOF promises.
Encoded tail replies are at most 1,402,200 bytes.

Preview defaults to 80 display columns by 24 rows. The maximum is 160 by 24.
Saved previews obey the requested dimensions, including wide Unicode text.
`live-preview` carries the worker's observation time and safe styled text when
available. `saved-preview` carries the actual checkpoint time, null live time and
plain text. Missing or corrupt screens return `state.unavailable`. Preview text
is limited to 32 KiB in aggregate and its encoded reply to 256 KiB.

Every output reply contains only the authenticated subject, session ID, format,
source, times, base64 tail or preview lines. It contains no recovery record,
recipe, path or agent resume data. Reads do not attach, resize a worker, send
input or recover a session. Legacy list, inspect, logs, attach, recovery, copy,
app controls, service changes and Mesh SSH stay denied for both permission sets.
`--max-bytes` is tail-only. `--cols` and `--rows` are preview-only.

## Revoke or rotate a credential

On the target:

```bash
mesh device scope revoke --identity PUBLIC_ID --expected-revision 1 --json
```

Replacing permissions or expiry, or revoking/regranting creates a new entry incarnation. Old
connections cannot adopt it. Unrelated grants and exact-content retries preserve
existing bindings. An idle watcher closes connections after observing invalidation.
Its polling interval is nominally 250 ms, not a real-time deadline. The target's
clock determines expiry; a live connection also has a monotonic lifetime cap.
Fresh handshakes and restarts rely on a reasonably correct target clock.

A failed durability acknowledgement may still leave a visible grant or revoke.
The result retains the known revision and expiry. Retry the same absolute change
to settle its barriers. Do not assume a failed revoke restored earlier access.

To rotate, create another credential directory, grant its new public identity,
then revoke the old identity. Do not copy the ordinary owner key into this workflow.

## Know the boundary

A scoped key is not an OS sandbox. A program with the daemon account's filesystem,
Unix socket or arbitrary execution access already has that account's powers.
Scope revocation cannot recall bytes already received. A read admitted earlier
may finish in memory; replies recheck authority before enqueue and actual write.
Bytes admitted to a write just before revocation may still arrive.

Ordinary private HTTP services and app/browser permissions have separate authority.
A deliberate HTTP publication may expose content or start a demand recipe.
This credential grants none of those permissions and does not revoke them.

Unsafe or malformed scope policy fails remote classification closed, including
ordinary full-device, updater and SSH admission. Same-account local access remains
available for backup and repair of `access/scopes.json`. The file is not reset to
an empty policy on error. See [device access](../concepts/device-access.md) for
full-device access and the accepted-work caveat.
