# Give each agent its own identity and contexts

Several agents can share one machine's Mesh key. Give each agent frontend its
own application identity, so its contexts and jobs stay private to it.

```bash
mesh mcp identity create codex
mesh mcp identity ls --json
```

The identity is a 256-bit random credential in a private file under the Mesh
config directory (`mcp/identities/NAME.json`, mode 0600). Mesh sends it only
inside controls on the authenticated connection or the daemon's local socket.
The host derives an application ID from the credential and the connection's
device key or account, and never stores the credential. To start over, create
an identity with a new name; an existing one is never replaced.

## Open a context

A context is a durable record on the host: a working directory, a shell for
command strings, an environment overlay and a label.

```bash
mesh context open --host HOST_ID --identity codex --request-key ctx-1 \
  --cwd /work/project --env RUST_LOG=debug --json
```

The reply carries a handle, `mc1.HOST_ID.CONTEXT_ID`, that later commands
accept in place of `--host`. Results name environment variables and a digest
of their values, never the values. The same key and options return the
original result of that open, even after the context changed or the host's
login shell moved. Different options under the same key return
`request.key_conflict`. If the reply is lost, look it up; this also returns
the original result, and `mesh context get` shows the current state:

```bash
mesh context receipt --host HOST_ID --identity codex --request-key ctx-1 --json
```

`--shell` is `login` (the default; the host's login shell runs `-l -c`), `sh`
(`/bin/sh -c`) or `none` (argv only). The login shell is resolved when the
context opens.

## Run jobs in a context

```bash
mesh session create --host HOST_ID --identity codex --context CONTEXT_ID \
  --kind pipe --request-key build-1 --shell-command 'make test' --json
mesh session request --host HOST_ID --identity codex --context CONTEXT_ID \
  --request-key build-1 --json
```

Context jobs are pipe jobs. `--cwd` is optional and relative paths resolve
against the context's directory. The host reserves the job against the
context's current revision, and the receipt records that revision. A later
edit or closure never changes the job or its replay. A child's `cd` or
`export` never changes the context.

Pass `--identity` and `--context` to `wait`, `logs`, `input`, `signal`, `kill`
and `rm` as well. Another identity gets `access.denied`, even with the exact
session ID and instance.

## Change and close

```bash
mesh context update CONTEXT_HANDLE --identity codex --expected-revision 1 \
  --cwd /work/project/app --set-env A=1 --unset-env RUST_LOG --json
mesh context close CONTEXT_HANDLE --identity codex --json
```

A stale `--expected-revision` returns `context.conflict`. Closing stops new
jobs (`context.closed`); running jobs keep going and keep their receipts.

A context expires 30 days after its last use, together with its open
receipt, once none of its jobs is running or uncertain (`runningJobs` and
`uncertainJobs`). After a reboot the context remains and its jobs show as
interrupted. Each identity can hold 64 open contexts per host, and a host
holds at most 1,024; an open whose reply was lost still holds its slot.
`mesh context ls` follows the host's pages for you.

## Account administration

Without `--identity`, the CLI administers the whole account, as before. For
raw job controls it says so explicitly, so a frontend that forgets its
credential is refused instead of acting as the account. An approved device
still administers its daemon account, and anything with direct access to that
OS account can read the account's data.
