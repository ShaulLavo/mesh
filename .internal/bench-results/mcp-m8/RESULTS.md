# M8 Hosted MCP Access Benchmark

Commit: `12462fd8f316e445364ac48b432874e6cc7271cb` (go version go1.27.4 linux/amd64, linux/x86_64, load avg 41.33/40.48/39.51).
Matched builds (`-tags mesh_integration`) against a compiled Go stateless `2026-07-28` MCP fixture (`echo` and 2 MiB `big` tool calls, exact output verified on every sample). Isolated test daemon on loopback `127.0.0.1` and separate client device identity; the live daemon was not touched.

| Mode | Cold start p50 | Cold start p95 | Warm call p50 | Warm call p95 | 2 MiB throughput | Processes | Frontend RSS | Daemon RSS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Direct local stdio | 1.11 ms | 1.26 ms | 0.01 ms | 0.02 ms | 409.4 MiB/s | 1 | 19360 KiB | — |
| Hosted (`mesh mcp connect`, local Unix) | 75.04 ms | 121.55 ms | 0.80 ms | 1.07 ms | 32.6 MiB/s | 4 | 58852 KiB | 75824 KiB |
| Hosted (`mesh mcp connect`, remote mTLS WS) | 153.64 ms | 228.71 ms | 2.06 ms | 2.77 ms | 38.4 MiB/s | 4 | 60868 KiB | 70940 KiB |

Raw samples: [hosted.json](hosted.json).
