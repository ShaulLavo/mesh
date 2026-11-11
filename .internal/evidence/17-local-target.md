# Explicit local target submission

The manual target path uses the host's authenticated planner and installation
engine. `DefaultHostMode` remains manual. Submission never changes policy mode
or predecessor settings.

## Local handler contract

Unit 9 authenticates an actual Unix-owner request before calling these APIs:

```go
(*HostUpdater).PlanLocalTarget(ctx, HostTargetSubmission) (HostTargetPlan, error)
(*HostUpdater).SubmitLocalTarget(ctx, HostTargetSubmission) (HostTargetStatus, error)
(*Store).HostTarget(ctx) (HostTargetStatus, error)
(*Store).CancelLocalTarget(ctx, expectedRevision uint64) (HostTargetStatus, error)
```

The exported input/status/evidence types and JSON tags are authoritative in
`internal/update/host_target_types.go`. Submission names an exact version, the
expected executing digest, expected policy revision and expected intent
revision. The first intent revision is 0. Optional `RetryEdge` names the exact
source, target and platform for a deliberate quarantine retry. No input carries
a trusted origin or an installation-approval credential.

Plan and submit share `planTarget`. Preview returns signed target and next-hop
evidence without a request ID, generation or reservation claim. It does not
invoke the helper or engine, change policy/intent/ledger, grant work, or send a
predecessor retention acknowledgement. It may refresh bounded authenticated
peer/discovery caches. Its ordinary-device evidence request has empty `Settled`
and `Drop=false`.

Submission verifies the source, exact target metadata and manifest, route,
forward/full rollback proof and retained workers. It checks the current helper
and records the private intent and exact owned request under authority before
returning success. The returned request/generation and target/next-hop pins are
the actual reservation. No installation journal is issued by the API itself.
The scheduler performs stage, retry and grant through the existing host engine.
An identical submission rejoins its original intent.

## Durable authority and cancellation

`updates/host-target.json` stores the explicit submission, immutable final
target pins, source progression, pending time and exact owned work. Ownership
references its intent revision. Every host acceptance requires both private
records, the current policy revision and the exact request/generation/edge.
Manual policy mode permits only this matching explicit intent. Missing or
malformed authority cannot become ordinary manual acceptance.

The installation lock precedes authority. Submit reserves under authority;
cancel uses authority alone and does not wait for the scheduler's download or
helper work. Cancellation increments the intent revision and fences all later
stage/retry/grant commits. A grant already issued finishes and is reconciled
with origin `manual` and its verified metadata pin. A changed policy revision
also fences the intent. Existing manual and coordinator controls remain.

The scheduler persists verified source progression between hops and stops at
the exact selected target. A newer latest release cannot change the intent.
Pending time survives polls and daemon replacement. The existing Unit 7 order
refresh and acceptance-time saved-order fence remain mandatory. A deliberate
local retry does not override a predecessor failure and does not erase local
quarantine before a successful commit.

The route cache is one bounded, source-and-target-digest-bound record. It holds
no permission. A different plan replaces it; every use reauthenticates signed
documents and exact proof. A crash after intent publication but before the
first ownership publication may restore that same reservation only while no
journal was accepted. Missing ownership beside an accepted matching journal
requires attention and cannot issue a grant.

## Adoption confirmation

Before committed-journal deduplication or a manual ledger append, reconciliation
checks the canonical adoption envelope under authority. It matches the exact
operation, pin, request generation, full request and installation settings
from the bounded operation-details projection. An unconfirmed matching
adoption yields. Unit 10 remains the sole writer of preserved-state/policy
validation, adoption-origin ledger evidence and confirmation. It publishes
those under the same authority lock. A later tick preserves that origin
through normal request/generation deduplication. Foreign journals remain
ordinary, unpinned, non-permitting history.

## Verification

Tests use authenticated signed fixtures, ordinary device evidence transport,
the actual engine, fake services and a real retained worker. Controls cover:

- Preview without helper/service/intent/ledger changes or retention ack.
- Durable reservation and idempotent replay before journal acceptance.
- Two manual hops across restart, unchanged policy, preserved worker I/O and
  a newer latest release that does not change the target.
- Cancellation during archive download and reconciliation after a grant.
- Policy/intent/current-digest races, missing ownership/intent and pre-journal
  crash recovery with the original request.
- Mandatory signatures, complete rollback, worker/helper checks and exact
  quarantine retry.
- Default absent manual policy and predecessor refusal at refresh and at the
  acceptance commit.
- Matching unconfirmed adoption before ledger dedup, followed by atomic
  adoption-origin publication; a foreign journal remains manual history.

Corrected signing `81f7efcd`, Unit 7 `a6c6c99e` including authenticated fixture
repair `8f05b6db`, and corrected rollback `b3adc107` are integrated unchanged.
Reports, exact checks and cleanup records remain under
`/work/reports/mesh-auto-update/unit6-host-updater/`. No live fleet, helper,
policy, identity, provider or OS configuration was changed.
