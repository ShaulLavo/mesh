# Mesh baseline — omarchy (x86_64)

Commit: `9e3f62b56405bdda7a90a32b0acfe3d628fd9208`. Go: `go version go1.27.0 linux/amd64`.

Warm cache, scratch Unix transport, one-core CPU percentage. WAL rates are short-window extrapolations.

| Sessions | Idle CPU % | Daemon RSS MiB | WAL commits/min | WAL frames/min | List JSON bytes | List ms | ls ms | version ms | Worker RSS MiB | Attach paint ms | Throughput MiB/s |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 0.033 | 39.93 | 59.9 | 179.7 | 72 | 0.16 | 29.89 | 20.40 | 0.00 | — | — |
| 5 | 0.166 | 40.23 | 59.9 | 359.5 | 11189 | 0.88 | 32.65 | 20.57 | 22.86 | 0.33 | 3.11 |
| 20 | 0.366 | 52.06 | 59.9 | 359.2 | 44499 | 2.65 | 41.72 | 20.85 | 23.02 | 0.63 | 3.70 |

| Sessions | ls CPU ms | ls peak RSS MiB | version CPU ms | version peak RSS MiB |
| ---: | ---: | ---: | ---: | ---: |
| 0 | 32.13 | 19.23 | 20.54 | 19.23 |
| 5 | 37.59 | 21.95 | 20.46 | 19.45 |
| 20 | 50.80 | 23.97 | 20.94 | 19.45 |

Historical provenance: the [independent review](https://github.com/ShaulLavo/mesh/pull/62#issuecomment-5935038472) observed ordinary binary SHA256 `d06c45e0fed1f624cdb4893c981f5d3a383f03a2d0b44d0f6566b15713b57a1a`, embedded `go1.27.0`, Linux/amd64 and `vcs.modified=true`; production blobs matched the immutable baseline. The ordinary artifact was later removed. Full original build settings and the dirty-worktree explanation were not captured. This is an attributed observation, not a complete newly verified build receipt. Measurement samples are unchanged.
