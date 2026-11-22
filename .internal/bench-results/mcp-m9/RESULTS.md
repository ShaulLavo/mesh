# M9 aggregate gateway benchmark

Built from `10cb0a3d` (after the review changes: every list rechecks the host
catalog, calls carry the resolved revision, listens watch host listing
revisions), `go1.27.4 linux/amd64`, `-tags mesh_integration`, on the PC
under load average 32. One isolated hosting daemon on loopback with its own
device key, ten shared stdio services of 50 tools each (the Python fixture
`integration/helpers/mcp_gateway_upstream.py`), and a separate client
device. The live daemon was not touched. Every sample checks its exact
output. Script: `scripts/bench/mcp_gateway.sh`.

| What | Direct `mesh mcp connect` | Gateway `mesh mcp --profile` |
| --- | ---: | ---: |
| Cold frontend start plus first call, p50 / p95 (8 samples) | 78 / 114 ms | 99 / 129 ms |
| Warm call, p50 / p95 (100 samples) | 58 / 92 ms | 55 / 107 ms |
| 2 MiB reply, p50 (10 samples) | 22.0 MiB/s | 17.8 MiB/s |

Discovery of 10 services and 545 tools (500 hosted, 45 native):

| | First list, empty cache | Fresh frontend, warm private cache |
| --- | ---: | ---: |
| All pages of `tools/list` | 5,784 ms | 46 ms |
| Second list in the same process | 29 ms | 38 ms |
| Catalog bytes sent | 281,313 | 281,313 |
| Frontend RSS / CPU | 47,844 KiB / 0.14 s | 47,628 KiB / 0.09 s |
| Host connections held | 1 | 1 |

The private cache holds 111,426 bytes. The daemon used 72,880 KiB RSS and
1.47 s CPU over the whole run. The empty-cache list includes starting all ten
on-demand services, two at a time per host.

A cold gateway call costs about 20 ms more than a direct one: it reads the
host catalog once to bind the call to the service's current revision. Warm
calls are within noise of each other. Each list now rechecks the host
catalog, which adds a few milliseconds to a cached relist (25 to 38 ms
here). Large replies run about 20% slower through the gateway, which
rewrites each result's resource URIs and server identity. The absolute warm
latency is high for both paths because of host load and the Python fixture;
the M8 benchmark with a compiled fixture measured 2 ms remote.

The first benchmark of this PR (before the review changes) measured 76 / 243
ms cold, 52 / 103 ms warm and 17.6 MiB/s through the gateway under load 28 to
36.

Raw samples: [gateway.json](gateway.json). Real client traces:
[clients-trace.txt](clients-trace.txt).
