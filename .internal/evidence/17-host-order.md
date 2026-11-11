# Durable predecessor order

Unit 7 keeps the manual default. No fleet policy or live service is changed.
Normal PC policy can name the Pi and VPS with a two-hour soak; the sleeping
Mac remains a separate requirement of initial adoption in Unit 10.

## Control contract

`protocol.TypeUpdateEvidence` is `update.evidence` and the reply type is
`update.evidence.result`. Both use `Control.Update` for the JSON body and
preserve `RequestID`. The ordinary authenticated device transport handles
identity and admission. Update-administrator enrollment supplies no evidence
access. A local Unix connection already has the daemon socket's local access.
The server derives the dependent identity from the authenticated connection,
never from the request body.

The authoritative request/reply types live in `internal/update/host_evidence.go`:

- `EvidenceRequest{Settled, AfterSeq, Limit, Drop, FailedAfter}`. `Limit` is 1
  through 64. `Settled` acknowledges the reader's committed version. `Drop`
  releases only that reader's retention. It cannot be combined with `Settled`.
- `EvidenceReply{Schema, ID, Mode, Phase, Entries, More, Failed, FailedNext,
  Attention, Adoption}`. Schema is 1. Entries and failed edges each obey the
  requested limit. `AfterSeq` resumes committed entries. `FailedAfter` and
  `FailedNext` resume failed edges independently.
- `Adoption` is the existing canonical `HostAdoption`, returned until that
  operation settles. Its operation, exact metadata pin, milestones and bounded
  opaque details remain independent of ordinary ledger pruning.

`Client.Evidence` uses full device admission and pins the endpoint identity.
It validates the response identity, correlation, fields, versions, metadata
pins and page progress. Only a connection failure is offline. Refusal,
unsupported controls and invalid replies persist as holds. An offline read
never clears an earlier hold, and no read result expires into permission.

## Store and CLI integration

`Store.HostEvidence(ctx, dependentID, request)` serves evidence under the
existing authority lock. The daemon supplies the reply ID. The ordinary
control never changes policy or installation authority.

`Store.HostDependents(ctx)` returns IDs, settled versions, last acknowledgement
and retained-entry counts. `Store.ForgetHostDependent(ctx, id)` releases one
retention record for Unit 9's local policy-forget command. Its caller must
provide the local authorization boundary. `Store.HostPeers(ctx)` returns the
last read state, time, hold, problem and durable page cursors for status.

`HostInfo` advertises `UpdateEvidenceSupported` and includes
`HostUpdate{Mode, Phase, Revision, Problem}`. These are read-only summaries.
Unit 9 owns the policy/status command and local mutation controls.

## Durable records and target selection

The predecessor registers an acknowledgement before pruning. It retains every
entry newer than any dependent's settled version, plus its latest entry.
Settlements cannot move backwards. Drops and local forget release retention.
The ledger head advances before old files are removed; repeating pruning also
removes an interrupted deletion. Request identity records remain tombstones,
so a pruned commit cannot be appended again after a crash.

The dependent keeps `host-peers.json` and individual bounded peer-entry and
failed-edge records in its existing private updates directory. Original reads
have no eight-entry truncation. Four pages per pass bound work, with durable
continuation across passes. Incomplete reads hold until completion. Entries
newer than the dependent's own version and its newest soaked entry remain;
settled older entries are pruned. Dropping a predecessor sends its drop
acknowledgement and removes local evidence after the acknowledgement succeeds.

For each configured predecessor, take the newest exact pinned commit whose
commit time is at least the configured soak ago. The minimum version of those
commits is the target limit. Histories need no common release. A newer canary
counts safely. Unpinned manual history is retained and paged but grants no
permission. Commits that did not preserve every retained worker also grant no
permission. Attention holds the dependent. Failed edges hold the exact source,
target and platform, including intermediate hops below the final limit.

The real daemon wires `HostOrder` to its saved identity fleet and device
client. `HostUpdater` refreshes peers on discovery and owned-request resume.
Discovery still traverses the stable latest root to the executing reader's
supported epoch. A newer predecessor's future-format documents remain unread.
Within the supported epoch, a lower limit selects its exact metadata SHA.
Root, signature, next-hop, full forward/rollback and retained-worker checks all
remain mandatory. The existing acceptance lock also checks saved predecessor
permission before stage, retry and grant commit. Policy revision fencing stays
in place.

## Verification

Focused race controls cover disjoint and alternating histories, an old soaked
commit followed by 280 commits inside one soak, continuation/restart, offline
progress, non-expiring refusal/unsupported/invalid holds, attention, failed
intermediate edges and platform isolation, acknowledgements, pruning, drops,
malformed replies, exact pins, signatures, rollback and withdrawal during an
archive download. Three isolated actual daemons exercise device-only evidence
access and the real protocol path. HostUpdater tests use controlled signed
release documents and the actual installation engine.

The corrected signing dependency requires authenticated public trust
provenance. The original-source executable fixture now overlays a generated
public TUF bootstrap, signed snapshot/targets and their exact trusted-root
bytes, as well as the reader epoch when needed. Production readers and signer
verification remain unchanged.
