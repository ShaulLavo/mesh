# Signed release verification

Unit 1 uses `github.com/sigstore/sigstore-go v1.4.0`, stable commit
`be417d95ee77934bb35e808d4f23728d13c70a72`. The dependency is approved for complete
bundle verification and authenticated TUF rotation. There is no custom
production certificate, timestamp, SCT, transparency-log or TUF signature
verification.

## Trust and signing

The bootstrap root is the public-good root embedded by that library. Its TUF
version is 14 and its expiry is June 22, 2026. Its SHA-256 is
`a0dfcc5d51c1ce4a66b541a3fff0afa97225ccc40456b21140bb2e2f113122e2`.
The embedded complete trusted material was fetched through the library's TUF
client from that bootstrap, including authenticated root continuity after its
expiry. Its SHA-256 is
`6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66`.
Both files are public trust material, not credentials.

`actions/attest` v4 is pinned at
`1e69f48acb82d1966a394da916b4c1698aa569d6`. The action signs the existing schema 1
manifest and archives. The publisher checks the original bundle with both
`gh attestation verify` and the candidate's `mesh-release-sign` command before
calling release finalization. No routing metadata is fabricated. Historical
unsigned sources have no signature reader; later route publication adds
verification by the updater-capable readers actually allowed on each edge.

The publisher compiles the candidate CLI and refreshes its authenticated trust
cache before wrapping. Exit status 75 means that a pass saved authenticated
roots and exhausted its document budget. Publication resumes that status only;
network, expiry and verification failures stop publication immediately.

Signing permissions are confined to the release publication job and the
push-to-main reusable caller. Pull request jobs receive no signing grants.
The leaf policy pins the GitHub issuer, exact main workflow, repository ID
1351591203, owner ID 68500209, main source ref, and GitHub-hosted runner.

## Reader API

- `EmbeddedTrustRoot()` returns the pinned public material and identity.
- `LoadTrustRoot(cache)` includes authenticated material previously persisted
  by a successful refresh. An empty cache path uses only embedded material.
- `RefreshTrust(ctx, cache, TrustRefreshBudget)` authenticates successive roots
  and the trusted-root target. A pass permits 8 to 64 document requests and at
  most 30 seconds. Eight requests let top-level metadata finish after the last
  root probe. Accepted roots are saved before the next request, so long root
  histories advance across passes. Current metadata expiry checks stay active.
- `VerifySignature(trust, subjectBytes, envelope)` verifies a SHA-256 subject.
- `VerifyArtifactSignature(trust, sha256, envelope)` supports streamed archives.
- `VerifyMetadata(trust, metadataBytes, envelope, manifestBytes, pageBytes)`
  checks the signed metadata and exact manifest and page digests. The schema 1
  manifest and forward receipt fields stay unchanged.
- `VerifyDigest(bytes, expected)` checks another asset bound by signed metadata.

The envelope retains the original bundle bytes. Limits are 256 KiB per
envelope, two entries, and 120 KiB per bundle, including unknown kinds.
Unknown kinds are skipped and a valid supported entry is required.

## Dependency cost

Measured on Linux amd64 with Go 1.27.4 at signing commit `b3002dcb`. The baseline
used the same source with the new signature and trust files overlaid as empty
packages and the pre-signing module files. Both builds used `-trimpath` and
`-ldflags='-s -w'`. This measures the maintained verifier's import cost before
automatic updates start using it.

| Artifact | Executable bytes | gzip 9 bytes |
| --- | ---: | ---: |
| Mesh baseline | 28,963,079 | 10,956,661 |
| Mesh with verifier | 40,354,055 | 14,783,159 |
| Signing CLI | 19,112,199 | 7,121,177 |

The increase is 11,390,976 executable bytes and 3,826,498 compressed bytes.
Warm build wall times were 13.629, 15.732 and 3.438 seconds respectively.
These are build observations, not speed benchmarks. The coordinator accepted
the material cost within the approved maintained-verifier choice.

Module selection also advances Cobra 1.9.1 to 1.10.2, pflag 1.0.6 to 1.0.10,
`x/crypto` 0.57.0 to 0.58.0, `x/net` 0.60.0 to 0.61.0, `x/sys` 0.48.0 to 0.49.0,
`x/time` 0.15.0 to 0.16.0, `x/sync` 0.23.0 to 0.24.0, `x/text` 0.42.0 to 0.43.0,
and the `x/exp` pseudo-version to `f0251eb4aa3e`. Other additions in `go.mod`
provide Sigstore protobuf, Rekor, SCT, timestamp and TUF verification. Test-only
signing helpers generate private fixtures; test roots are not embedded in the
shipped binary. The separately owned Go 1.27.4 directive change is not duplicated.

## Verification

Run under `GOTOOLCHAIN=go1.27.4` with caches on `/work`:

```sh
go test -race ./internal/release ./cmd/mesh-release-sign
go mod tidy -diff
scripts/gates.sh
scripts/check-packaging.sh
python3 scripts/test-release-signing.py
go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
```

All these commands pass. The vulnerability scan reports zero called
vulnerabilities, with two uncalled imported-package advisories and one uncalled
module advisory. Recorded public-good evidence verifies offline with an expired
May 2024 leaf. Generated fixtures exercise valid SCT and Rekor signatures,
expired leaves, observer timestamps, wrong CA and log keys, exact identities,
altered metadata and assets, wrapper bounds, authenticated rotation, expired
bootstrap continuity and budget resumption. Workflow shell-step tests prove
that official verification, wrapping and candidate verification failures leave
the draft unpublished.

The broad `go test -race ./...` run exited 1. Existing CLI policy fixtures failed
with `domain policy changed`; the coordinator assigned those controls to PR 430.
Three other deadline-sensitive failures in fleet sharing, host metrics and
automatic-update tests passed their focused race rerun without changed deadlines.
The broad integration run exited 1 on `bench_harness.sh` at its 30-second budget.
The same harness passed alone through the same controller and unchanged budget.
The broad suites were not rerun. Their red results remain part of the evidence.

Local runner records, failure excerpts and costs are under
`/work/reports/mesh-auto-update/unit1-signing/`. The fixture helper's log-ID
serialization bug is recorded in the upstream-bugs report. The test decodes
its hexadecimal ID before serialization; production verification is unchanged.

## Cleanup

Tests created no services, containers, mounts, routes or fleet changes. Runner
records report zero leftover processes and zero OOM kills. Temporary signing
trust caches, generated certificates, binaries and build overlays were removed.
The timed-out benchmark left a 55 MiB scratch directory containing only its
build output. After confirming there were no process or cwd consumers, the
worker removed that exact directory and checked its absence. Source fixtures
and the small report directory remain for review. The coordinator owns their
retention and the prepared worktree's retirement after merge. Existing Go
module and build caches remain shared active caches.

## Primary sources

- [Sigstore Go verification](https://github.com/sigstore/sigstore-go/blob/v1.4.0/docs/verification.md).
- [Sigstore TUF client](https://github.com/sigstore/sigstore-go/blob/v1.4.0/pkg/tuf/client.go).
- [TUF root verification](https://github.com/theupdateframework/go-tuf/blob/v2.4.2/metadata/trustedmetadata/trustedmetadata.go).
- [Pinned attest action](https://github.com/actions/attest/blob/1e69f48acb82d1966a394da916b4c1698aa569d6/action.yml).
- [Recorded public bundle](https://github.com/cli/cli/blob/233b470d3a189e89cf6fa69624e11c3fa9387d81/pkg/cmd/attestation/test/data/reusable-workflow-attestation.sigstore.json).
