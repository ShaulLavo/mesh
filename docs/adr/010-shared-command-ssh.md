# Share command SSH transports through a private helper

Pipe-mode `mesh ssh` calls use a private helper for each local state directory.
The helper retains authenticated SSH transports across CLI processes. Interactive
shells keep their own connection.

Repeated short commands spend time authenticating a new transport. Reuse removes
that work while preserving a channel and process for each invocation. The local
daemon is a separate coordinator with its own lifecycle and configuration. A
command helper keeps outbound connection ownership independent of that daemon.

The helper accepts same-user Unix connections through a private socket. Each
request passes three private pipe descriptors and a bounded structured launch.
The CLI forwards output from those pipes and waits for both EOFs before returning
status. Stdin EOF closes only remote stdin. Losing the control connection cancels
only the invocation's channel and remote process group.

Connection keys include the pinned destination, endpoint, port, and fingerprint
of the caller key snapshot used to authenticate. Each invocation retains current
target verification. The server continues observing device grant revocation.
Channel rejection and command exit leave a healthy transport usable. A failed
transport is discarded for future calls. Commands are never replayed.

Limits cover destinations, channels, accepted local requests, descriptors, header
bytes, setup time, and idle lifetime. A shared dial belongs to the pool, so one
waiter's cancellation cannot cancel other waiters. Map locks cover bookkeeping;
dialling, command execution, and stream forwarding run independently.

SSH has no cancellation message for an unconfirmed channel open. Cancelled setup
remains accounted until the reply arrives or the transport closes. There are at
most 128 setup operations across all keys, including connections being reclaimed.
A transport with abandoned setup closes when its last active user leaves. Late
channel acceptance is closed without a launch. An already-sent execution request
has an uncertain outcome and is never replayed.

The helper uses memory and descriptors while idle. It closes idle connections and
exits after one unused minute. It installs no service. The existing server
admission policy remains in force and can reject bursts even on a reused transport.
