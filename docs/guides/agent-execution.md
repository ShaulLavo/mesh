# Run a keyed detached command

Save a request key before sending a command. Use that key to find the accepted
session if the connection or its reply is lost.

```bash
mesh session create --host HOST_ID --kind pipe --request-key job-17 \
  --cwd /work/project --json -- /usr/bin/git status --short
mesh session request --host HOST_ID --request-key job-17 --json
```

The target must already be online and initialized. These commands do not wake,
install, or start a daemon. They use the existing authenticated connection and
never fall back to a local launch or cached execution result.

## Keep the original key

An equal create retry returns the original session and instance. Different
arguments, working directory, session kind, or stdin mode conflict with that
key. For `--kind pty`, terminal settings are part of the request. The defaults
are 80 columns, 24 rows, and `TERM=dumb`. Commands run as an argument array.
Use an explicit shell argument when shell syntax is needed.

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
signal. Durable wait and instance-bound controls use the saved binding. Use
`mesh session inspect --instance INSTANCE` to read a keyed session without
attaching. A pipe session that had started before a reboot reports an interrupted
result. A dispatch without evidence of a start stays uncertain.

Active and uncertain receipts have no automatic expiry. Definite terminal
receipts remain for at least 30 days of confirmed elapsed time. Clock jumps do
not prove that floor. Missing clock evidence or a reboot can extend retention.
Saved recovery history can pin a receipt longer. The UTC retention time is an
earliest candidate, not permission to reuse an old key.

The journal accepts at most 10,000 retained records. Active or unresolved
attempts also reserve one of 128 logical 16 MiB output slots. Terminal output
can be evicted to free a slot without removing its receipt or saved recovery.
Quota refusal starts no command. Existing keys remain readable at quota.

Ordinary removal must preserve the receipt and wait until the exact worker has
finished every pathname operation. Offline forgotten markers cannot bypass this
protection. Removing a finished payload keeps the compact result and original
key until retention permits expiry.

## Read exact output and completion

Save the session ID and instance from the creation receipt. Pipe jobs have
independent `stdout` and `stderr` streams. A PTY job has one `pty` stream that
includes the terminal's line handling.

```sh
mesh session logs --host HOST_ID 7K3D --instance INSTANCE --stream stdout --json
mesh session wait --host HOST_ID 7K3D --instance INSTANCE --timeout 5s --json
mesh session signal --host HOST_ID 7K3D --instance INSTANCE --signal term --json
mesh session kill --host HOST_ID 7K3D --instance INSTANCE --json
mesh session rm --host HOST_ID 7K3D --instance INSTANCE --json
```

Decode `bytesBase64` to get bytes. Save `nextCursor` for that reader's next page.
Each reader has its own cursor. Default pages contain at most 64 KiB of decoded
bytes. `--max-bytes` accepts 1 through 1,048,576. Offsets inside cursors and final
ranges are decimal strings, so clients must not convert them to floating point.
An empty page at the current end is not EOF while the stream remains open.

Capture retains the most recent 8 MiB of each pipe stream or 16 MiB of PTY
output. A slow reader can receive
`logs.cursor_expired` with a gap and the earliest available cursor. A cursor for
another instance or generation also expires. A future offset returns
`logs.cursor_invalid`. Missing or damaged storage returns unavailable. It never
falls back to diagnostic logs or a rendered screen. Queue overflow and storage
loss are explicit. There is no successful page that joins bytes across a gap.

Wait returns `completed` only after the final manifest and result are durably
confirmed. It returns `timed_out` with the current receipt when its finite wait
ends. Waiting, reading, and cancelling either operation leave the child alive.
The CLI reserves a small part of its deadline to receive the timeout response.
A crashed worker can leave a durable output prefix and an unknown result.
Descriptor-holding descendants have the existing 250 ms post-leader drain bound.
A drain cutoff reports `eof: false` and `closeReason: drain_timeout`.

Signal and kill require the exact instance and a worker acknowledgement. A lost
acknowledgement leaves delivery uncertain. Kill intent is durable before delivery.
Removal requires terminal evidence and the worker's released finalizer fence.
It expires detailed output and results while retaining the compact receipt.
Output can expire under the capture quota or seven-day retention policy.
Reading expired pipe output returns `result.expired`. The compact receipt
retains the exit details.

## Write to pipe stdin

Pipe jobs read from `/dev/null` by default. Pass `--stdin open` when creating a
job that needs input. Save a key for each logical write and submit the expected
byte offset. The first offset is zero. For the next write, add the acknowledged
length to the previous offset.

```sh
mesh session input --host HOST_ID 7K3D --instance INSTANCE \
  --write-key line-1 --offset 0 --text 'hello' --json
mesh session input --host HOST_ID 7K3D --instance INSTANCE \
  --write-key end --offset 5 --eof --json
```

Use `--base64`, `--file PATH`, or `--file -` for binary input. Each write can
contain up to 1 MiB. `--eof` closes the pipe after its bytes, including when the
write is empty. The receipt confirms admission to the worker's buffer. The
process can exit without reading every accepted byte.

If a reply is lost, pass `--write-key` without `--offset` to look up the
acknowledgment. An exact retry returns the same acknowledgment. If the worker
cannot confirm acceptance, Mesh reports `input.outcome_unknown`. Keep the
original key. If Mesh reports `input.backpressure`, the buffer had no room for
that write and you can retry it with the same key and offset.
