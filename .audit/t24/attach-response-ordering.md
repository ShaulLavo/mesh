# Attach rejection ordering

The retained run of `scripts/check-t24.sh` failed its second SSH recovery attachment with `daemon: session AYT7 is not attached`, although the worker rejected that attachment with `ReasonAttached`.

`clientRelay.attach` queued the worker's rejection in the relay output FIFO. The same client's initial terminal resize then failed because no relay lane had been attached. `clientServer.Handle` wrote that error directly through `serializedClientConn`. The mutex serialized socket writes but allowed the direct resize error to overtake the queued worker rejection. The CLI consumed the first error, losing the ownership reason.

Fail-before command:

```text
go test ./internal/daemon -run TestClientServerKeepsAttachRejectionBeforePipelinedResizeError -count=1
--- FAIL: TestClientServerKeepsAttachRejectionBeforePipelinedResizeError (1.00s)
    server_test.go:229: timed out waiting for request processing while rejection and resize responses are queued
FAIL github.com/shaul/mesh/internal/daemon 1.019s
```

The regression blocks an existing stream's output, submits a detached-only attach that the worker rejects, then pipelines a resize and host-info request. The blocked writer must not stop the reader. On release, output must contain the original stream data, the ownership rejection, the resize error, and the host-info reply in that order.

The fix sends daemon replies and errors through the relay's existing FIFO and removes the alternate writer mutex. Control response bytes now count against the queue budget, with 64 KiB reserved for controls beyond its 8 MiB data budget. A separate check proves large queued control responses stay bounded and release their byte budget after writing.

Pass-after command:

```text
go test -race ./internal/daemon -run 'TestClientServerKeepsAttachRejectionBeforePipelinedResizeError|TestClientRelayBoundsQueuedControlResponseBytes|TestClientServerSerializesLifecycleAndRelayWrites' -count=1
ok github.com/shaul/mesh/internal/daemon 1.210s
```

Nearby verification:

```text
go test -race ./internal/daemon ./internal/transport ./internal/cli ./internal/sshd
ok github.com/shaul/mesh/internal/daemon 5.936s
ok github.com/shaul/mesh/internal/transport (cached)
ok github.com/shaul/mesh/internal/cli 3.135s
ok github.com/shaul/mesh/internal/sshd (cached)
go vet ./internal/daemon
exit 0
golangci-lint run ./internal/daemon/...
0 issues.
```

The rebuilt binary passed ten consecutive runs of the real OpenSSH integration:

```text
go build -tags mesh_integration -o /tmp/mesh-t24-relay-stress ./cmd/mesh
for trial in {1..10}; do python3 integration/helpers/recovery_transactions.py ssh /tmp/mesh-t24-relay-stress || exit 1; done
```

All ten runs printed `PASS: real OpenSSH shares daemon recovery, preserves ownership and argv, and requires explicit command execution`. The ownership assertion remains exact; it requires the replacement ID and the worker's attached rejection.

The root agent independently reran all ten iterations after final verification.
Raw commands and output are retained in `/tmp/mesh-t24-ssh-stress.log`.
