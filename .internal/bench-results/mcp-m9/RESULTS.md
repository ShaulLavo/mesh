# M9 aggregate gateway benchmark

Built from `eaafb2e0` plus the uncommitted gateway branch work (the same
tree as the PR's second commit), `go1.27.4 linux/amd64`, `-tags
mesh_integration`, on the PC under load average 28 to 36. One isolated
hosting daemon on loopback with its own device key, ten shared stdio services
of 50 tools each (the Python fixture `integration/helpers/mcp_gateway_upstream.py`),
and a separate client device. The live daemon was not touched. Every sample
checks its exact output. Script: `scripts/bench/mcp_gateway.sh`.

| What | Direct `mesh mcp connect` | Gateway `mesh mcp --profile` |
| --- | ---: | ---: |
| Cold frontend start plus first call, p50 / p95 (8 samples) | 112 / 168 ms | 76 / 243 ms |
| Warm call, p50 / p95 (100 samples) | 54 / 98 ms | 52 / 103 ms |
| 2 MiB reply, p50 (10 samples) | 18.6 MiB/s | 17.6 MiB/s |

Discovery of 10 services and 534 tools (500 hosted, 34 native):

| | First list, empty cache | Fresh frontend, warm private cache |
| --- | ---: | ---: |
| All pages of `tools/list` | 5,185 ms | 34 ms |
| Second list in the same process | 26 ms | 25 ms |
| Catalog bytes sent | 267,520 | 267,520 |
| Frontend RSS / CPU | 48,408 KiB / 0.13 s | 44,772 KiB / 0.06 s |
| Host connections held | 1 | 0 |

The private cache holds 107,415 bytes. The daemon used 55,444 KiB RSS and
1.39 s CPU over the whole run. The empty-cache list includes starting all ten
on-demand services, two at a time per host.

The gateway adds no measurable cost over direct connection on this host: the
warm and large-reply numbers are within noise of each other. The absolute warm
latency is high for both because of host load and the Python fixture; the M8
benchmark with a compiled fixture measured 2 ms remote.

Raw samples: [gateway.json](gateway.json).
