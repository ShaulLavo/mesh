# Mesh baseline — pi (aarch64)

Commit: `9e3f62b56405bdda7a90a32b0acfe3d628fd9208`. Go: `go version go1.27.0 linux/amd64`.

Warm cache, scratch Unix transport, one-core CPU percentage. WAL rates are short-window extrapolations.

| Sessions | Idle CPU % | Daemon RSS MiB | WAL commits/min | WAL frames/min | List JSON bytes | List ms | ls ms | version ms | Worker RSS MiB | Attach paint ms | Throughput MiB/s |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 0.332 | 31.41 | 59.8 | 179.4 | 72 | 1.70 | 703.59 | 692.33 | 0.00 | — | — |
| 5 | 1.233 | 34.84 | 60.0 | 359.8 | 11050 | 7.38 | 723.27 | 692.34 | 23.56 | 3.11 | 0.41 |
| 20 | 3.985 | 42.20 | 59.8 | 358.6 | 43941 | 25.52 | 775.50 | 692.95 | 23.75 | 3.31 | 0.39 |

| Sessions | ls CPU ms | ls peak RSS MiB | version CPU ms | version peak RSS MiB |
| ---: | ---: | ---: | ---: | ---: |
| 0 | 709.22 | 15.50 | 697.48 | 15.05 |
| 5 | 737.15 | 19.68 | 697.41 | 15.42 |
| 20 | 795.62 | 21.99 | 698.40 | 15.67 |

Historical provenance: the [independent review](https://github.com/ShaulLavo/mesh/pull/62#issuecomment-5935038472) observed ordinary binary SHA256 `e09fbcd9ce2017b50db2df9fbf28e41c044ec772fa9a0cb91128fb48f6275b6e`, embedded `go1.27.0`, Linux/arm64 and `vcs.modified=true`; production blobs matched the immutable baseline. The ordinary artifact was later removed. Full original build settings and the dirty-worktree explanation were not captured. This is an attributed observation, not a complete newly verified build receipt. Measurement samples are unchanged.
