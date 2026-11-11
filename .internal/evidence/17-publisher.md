# Automatic update publisher

Unit 3 implements the release policy and publication producer. The PR remains a
draft until the source-reader contract and corrected shared signing and rollback
source arrive from the coordinator. No release or fleet update ran.

## Policy and graph

The public inventory on November 11, 2026 contains 289 releases and 250 schema 1
manifests. The publisher verifies 5,879 platform-specific edges from actual
receipt bytes. The longest verified path to v0.1.306 is 32 hops.

The derived seed ladder has 19 releases. Its oldest is v0.1.179, published
October 4, 2026 at 09:33:37 UTC. For the recorded candidate time on November 11,
the recent window contains 16 anchors and starts at v0.1.203, published October
5 at 18:35:44 UTC. The first policy release proves every seed, including the
three earlier seeds outside that recent window.

`scripts/upgrade-policy.json` records the seed candidates. Native Linux proofs
passed for all 19. Darwin arm64 and Linux arm64 validation remains required in
CI before this list is accepted. The native workflow builds each platform's
candidate, startup-fault fixture, boundary tool, and worker test executable
once, then reuses those executables for every source.

Sixteen historical sources produce forward receipts only. Fleet sources
v0.1.249, v0.1.255, and v0.1.257 require separate rollback proofs and real
startup faults. The Linux proof run took 125.75 seconds in total. Its full
proofs took 28.66, 28.66, and 38.08 seconds. No sharding is needed at that
measurement. The workflow retains per-source timings for the other platforms.

The pre232 verifier reads the actual historical
`retainedOpenedCandidateState` contract. Its recorded v0.1.52 to v0.1.60
fixture fails the current receipt reader and passes the named historical
reader. Historical evidence grants no automatic permission. The rejected
v0.1.196 rollback edge remains excluded.

## Publication checks

The producer binds exact manifest bytes, platform, artifact and executable
digests, source capabilities, and content-addressed receipts. Missing source
archives block publication after bounded acquisition. Retained assets come
from paginated release and asset APIs. Missing API digests use a bounded
download, with cache identity tied to the immutable asset ID and update time.
The second inventory acquisition verified 6,258 actual cache hits.

Prior full pages retain their original references. New partial pages contain
only earlier published anchors. The current anchor enters the next page.
New epochs require an old-format bound waypoint, represented at ordinal zero
in the new epoch. The schema 1 manifest and forward receipt formats stay
unchanged. The manifest producer enforces the 512 KiB output bound.

The release job signs the metadata and retention inventory, checks the original
Sigstore bundle, refreshes candidate TUF trust, and verifies metadata binding.
Only durable refresh budget status 75 resumes. Other failures leave the draft
unpublished. The existing source/version reservation and serialized finalizer
remain in control. No script manually publishes a release during development.

The native proof workflow includes an original v0.1.249 reader gate. It checks
the generated manifest, exercises the original receipt decoder through its
bridge review, and refuses a corrupt receipt. Its local positive control used
the actual published v0.1.257 manifest and receipt bytes. The gate stops at the
installation review because the disposable reader has no installed helper.
That control certifies decoding, not adoption or an installed update.

## Validation and remaining dependencies

Focused Go 1.27.4 race tests passed in `cmd/mesh-release-routes`,
`internal/release/cmd/manifest`, `cmd/mesh-release-sign`, and `internal/release`.
Full quality gates passed with zero new or stale findings and no suppression
or baseline additions. Packaging checks and the acquisition, baseline,
transition, signing, and CI-download tests passed.

Actionlint 1.7.12 accepts all changed workflows after omitting the pre-existing
`concurrency.queue: max` line from its input. That installed version does not
recognize the existing GitHub queue field. The workflow retains the field.

Final validation must consume the coordinator's corrected trust-cache and
rollback source. The signed capability marker and actual original source
binary verifier still require an ownership agreement with Unit 6. Metadata
presence must not certify an automatic reader. Candidate signatures and all
three native platform gates remain required before publication.

Local commands and small results are retained under
`/work/reports/mesh-auto-update/unit3-publisher`. Task scratch roots were removed
after their processes stopped. The coordinator owns the prepared tree and
report retirement after review. Shared Go caches and the immutable public
evidence and baseline caches remain active consumers for CI reproduction.
