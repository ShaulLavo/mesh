# Complete rollback journal context

The Darwin v0.1.249 proof in run 43151893803 failed before establishing a
rollback compatibility result. Its fixture wrote only `compatibility` inside
`Request.Manifest`. The original source's cache-retention decoder requires the
complete schema 1 manifest and correctly refused schema zero.

Full proofs now require `MESH_ROLLBACK_MANIFEST`, an absolute path to complete
candidate context. Unit 8 can supply its existing complete three-platform
fixture manifest. The producer checks actual candidate version, commit,
platform, executable digest, state and worker writes, and actual source ranges
before starting proof daemons. The journal retains the complete supplied
manifest, without rewriting its artifact identities.

Startup-fault companions remain distinct from the real release image. The
fixture receives both paths, checks their shared build identity and capabilities,
copies the actual failed image into the isolated installation, and records its
digest separately. It never substitutes that companion digest for the real
release's artifact. Existing helper restoration, startup/cache validation,
worker identities, token I/O, boundary and recovery assertions remain enabled.

Fresh native CI preparation records one tiny `rollback-artifact.json` per
platform from actual candidate and source reports and the actual archive bytes.
An existing final archive is reused. PR preparation creates an actual archive
from its once-built candidate. A metadata-only job combines those three records
into an unpublished context with a provisional source-to-candidate read range
and no transition claims. It is not a published release manifest or an
installation authorization. Final publication still derives compatibility and
transitions from completed receipts. No additional native candidate compile or
multi-gigabyte artifact download is needed for context aggregation.

The four measured preparation tool hashes stay unchanged in meaning. Existing
prepared tools can consume the new producer without a mandatory new tool flag.
The boundary command's optional `-verify-manifest` is a decoder control, not a
version fallback.

## Bounded controls

Repository-owned cache-decoder controls reproduce the compatibility-only
schema-zero rejection and accept complete actual manifest data through the
real `PruneCache` path. Producer tests check identity binding and refusal of
instrumented bytes presented as the release image. Metadata aggregation tests
reject missing or mismatched platform records and emit no transition claims.

A bounded Linux control restored the original published v0.1.249 daemon with
the original uninstrumented published v0.1.306 helper at the first schema 13
boundary. Its context uses actual v0.1.306 artifacts and an explicitly
unpublished provisional range covering the actual source schema. The original
published manifest was not altered or described as having that range. The
control ran the existing source inventory, worker/shell identity, session I/O,
recovery and helper rollback checks. It emitted no full transition receipt.
Scratch and process consumers were checked absent on the actual host.

The Darwin v0.1.243 forward-only observer failure is a separate unresolved
gate. Observer nonzero status now retains bounded actual stdout and stderr,
with a failure-output regression and the original waits unchanged. Live
inventory is not mapped-image proof. No cause or native success is claimed
until the real observer context supplies its result.
