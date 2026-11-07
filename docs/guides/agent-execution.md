# Run a keyed detached command

Save a request key before sending a command. Use that key to find the accepted
session if the connection or its reply is lost.

```bash
mesh session create --host HOST_ID --kind pty --request-key job-17 \
  --cwd /work/project --json -- /usr/bin/git status --short
mesh session request --host HOST_ID --request-key job-17 --json
```

The target must already be online and initialized. These commands do not wake,
install, or start a daemon. They use the existing authenticated connection and
never fall back to a local launch or cached execution result.

## Keep the original key

An equal create retry returns the original session and instance. Different
arguments, working directory, terminal geometry, or terminal type conflict with
that key. Defaults are 80 columns, 24 rows, and `TERM=dumb`. Commands run as an
argument array. Use an explicit shell argument when shell syntax is needed.

The request namespace includes the host and connection identity. Local Unix
requests share the daemon account's namespace. Remote requests use the verified
Mesh device key. Changing between those paths changes the namespace, so an absent
request on another path does not prove the command was never accepted.

A lost reply never authorizes a new logical key. An uncertain dispatched command
is not launched again, including after a daemon restart or known reboot. A
reserved command whose coordinator disappeared before dispatch becomes
`not_started`. That result consumes its key too. Mesh does not guarantee that
external side effects occur exactly once.

## Read the receipt

Each invocation prints one JSON envelope. A known accepted or unconfirmed receipt
with an operational diagnostic has status `partial` and CLI exit 3. Keep its
binding and query the original key. A command's exit code is result data, not the
Mesh CLI's exit code.

The receipt keeps publication separate from launch outcome. Catalog failure does
not cancel accepted work. A terminal result records its actual exit code or
signal. Output capture is currently `not_recorded`; this interface does not
provide exact logs, durable wait, pipe sessions, stdin feeds, or instance-bound
signal controls.

Active and uncertain receipts have no automatic expiry. Definite terminal
receipts remain for at least 30 days of confirmed elapsed time. Clock jumps do
not prove that floor. Missing clock evidence or a reboot can extend retention.
Saved recovery history can pin a receipt longer. The UTC retention time is an
earliest candidate, not permission to reuse an old key.

The journal accepts at most 10,000 retained records. Active or unresolved PTY
attempts also reserve one of 128 logical output slots. Terminal results release
that active slot even when the command finishes before the first reader.
Quota refusal starts no command. Existing keys remain readable at quota.

Ordinary removal must preserve the receipt and wait until the exact worker has
finished every pathname operation. Offline forgotten markers cannot bypass this
protection. Removing a finished payload keeps the compact result and original
key until retention permits expiry.
