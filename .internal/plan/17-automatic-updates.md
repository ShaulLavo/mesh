# Mesh updates itself everywhere

Status: Approved (owner, 2026-11-10). High priority. Design written 2026-11-11
against main `514700f4` (v0.1.306) and corrected after the independent review
of `a47e958e`. The [review resolution](#review-resolution) maps each finding to
its section.

This is in-repository plan 17. It replaces the binary rollout steps of
[14: keep the fleet current](14-fleet-auto-updates.md) (steps 1, 2, 3 and 6)
and builds on [T26](../tasks/T26-mesh-updates.md). Plan 14's configuration
delivery and drift presentation stay. The "audit Plan 017" that plan 14 cites is
a different document: the audit wave that built the consent-bound coordinator
policy.

## Goal

Nobody updates Mesh by hand. When a release is published, every machine in the
fleet (PC, Mac, Pi, VPS and any machine added later) moves to it on its own.

On 2026-11-10 the PC and the Mac ran v0.1.249 while the latest release was
v0.1.306. The Pi and the VPS ran v0.1.255. `mesh update` to the latest release
refused, because v0.1.306 lists no tested transition from those builds and
reports a state read range of 15 to 16 against their state version 13. Catching
up meant finding and installing intermediate releases by hand. The Mac went
from v0.1.249 to v0.1.257 that way.

### Done means

1. Publishing a release is enough. Each machine notices it and updates with no
   command, prompt or approval.
2. A machine that was off or offline for weeks catches up on its own when it
   returns, however many releases it missed. Mesh plans and runs the chain.
3. Running sessions survive every update, including the sessions and services
   the PC hosts. A failed update rolls back on its own, is reported, and leaves
   the machine with a working Mesh.
4. Release CI refuses to publish a release that leaves any supported older
   release without a tested upgrade path to it.
5. The managing chat's hourly check sees every machine's Mesh version and is
   told when one has fallen behind or an update failed.
6. CI runs the real thing: an old installed release jumps several releases to
   the candidate with a live session that must survive.

### Constraints

- Releases are signed, and every unattended installation verifies the
  signature against a trust root pinned in the verifying executable. The fixed
  HTTPS origin, artifact hashes, exact tested transitions and signed update
  controls from enrolled administrators all stay.
- New machines enter the same automatic policy.
- No real fleet daemon changes before the rollout below. The PC hosts the model
  proxy and the managing chat. It is adopted last, automatically, after the
  other three machines have updated unattended and managing has been notified.
  Nobody runs an update command on any machine.
- The owner allows compatibility code for the supported-older-release
  requirement. The maintained `github.com/sigstore/sigstore-go` verifier is
  permitted for complete signature and trust-rotation verification.

## What exists

The single-host transaction is solid and stays.

- **Release identity.** `internal/release/release.go` defines `Manifest`,
  `Artifact`, `Compatibility` and `Transition`. `Manifest.Allows`
  (`release.go:132`) admits an installed build only when the manifest lists a
  transition from that build's exact executable digest and the build's state
  and worker versions fall inside the manifest's ranges. A version label never
  authorizes anything.
- **Release origin.** `release.Client` (`internal/release/client.go`) reads
  `mesh-release.json` and archives only from
  `https://github.com/ShaulLavo/mesh/releases` over HTTPS, bounds every reply,
  and checks archive and executable SHA-256. Release artifacts carry no
  cryptographic signature. The signatures in Mesh updates are on update
  controls (`update.Message.Sign`, `internal/update/auth.go:47`), accepted from
  the host's own identity or an enrolled administrator
  (`Authority.IsAdministrator`, `auth.go:297`).
- **Installation.** `updateinstall.Engine` (`internal/updateinstall/engine.go`,
  `transaction.go`) stages a verified candidate, takes an activation grant, and
  hands activation to a separate helper service. The helper stops the daemon,
  switches the executable, starts the candidate, and checks the executing
  digest, the database version, and every retained worker's PID and shell PID
  (`verifyHealth`, `transaction.go:293`). A failed check restores the previous
  executable (`restore`, `transaction.go:223`). The journal survives reboot.
- **The helper as supervisor.** The helper runs under its own service unit and
  outlives the daemon. It already resumes durable work that the daemon cannot
  own (`updateHelperStep`, `internal/cli/update_helper.go:134`;
  `resumePendingCoordinatorSetup`, `update_coordinator_setup.go:150`), and a
  helper newer than its daemon is an accepted state
  (`reviewBridgeHelper`, `internal/updateinstall/bridge.go:69`).
- **Local signed controls.** Software running in the daemon's account signs
  `update.control` messages with the host identity key and sends them over the
  Unix socket (`update.Client.Call`, `internal/update/client.go:37`;
  `LocalHost`, `local.go:16`). The daemon stages and grants on them
  (`updateController.install`, `internal/daemon/update.go:116`).
- **Publication.** `.github/workflows/release.yml` reserves a version, builds,
  proves a forward upgrade on native linux/amd64, linux/arm64 and darwin/arm64
  runners (`scripts/prove-release-transition.sh`), assembles compatibility from
  the receipts (`scripts/assemble-release-compatibility.py`), and publishes.
- **Release checks.** The daemon refreshes the latest manifest every six hours
  plus jitter (`internal/updatenotice/notice.go:18`, started at
  `internal/cli/command.go:1784`) and saves it as `CheckedRelease`.
- **Coordinator policy.** v0.1.295 added a consent-bound automatic policy
  (`internal/update/automatic*.go`, `mesh update automatic`). One coordinator
  pins one release for a saved fleet and pushes it to recipients that each
  authorized that exact fleet and epoch.

## What is broken

Findings were read from published manifests and from the PC on 2026-11-11. The
scripts and raw output are in
`/work/reports/mesh-auto-update/design-evidence/`.

1. **Manifests claim a chain, and nothing walks it.** Every release with a
   manifest (v0.1.41 onward, 250 releases) lists transitions that form a chain
   to v0.1.306 on all three platforms. From v0.1.249 it is seven hops: 249,
   257, 266, 274, 284, 293, 304, 306. From v0.1.255 it is six: 255, 264, 272,
   281, 291, 302, 306. From v0.1.257 it is six: 257, 266, 274, 284, 293, 304,
   306. The evidence script read manifest claims and ranges. It did not fetch
   or verify receipt bytes. Unit 3 rebuilds the graph from verified receipts
   before any seed is committed.
2. **Receipts follow two contracts.** Releases from v0.1.232 write
   `candidateOpenedRetainedState`. Earlier ones write
   `retainedOpenedCandidateState`. `VerifyTransitionReceipt`
   (`internal/release/bridge.go:181`) decodes strictly and accepts only the
   current contract, so receipts published before v0.1.232 do not verify with
   today's code.
3. **Each release proves eight predecessors.**
   `scripts/prepare-release-baselines.sh:59` requires only the previous release
   and adds the next seven when their archive downloads. 50 releases were
   published between v0.1.249 (October 17) and v0.1.306 (November 8), so a
   build leaves direct reach in about three and a half days.
4. **The optional sources drop silently.** v0.1.272 has no linux/amd64
   transition from v0.1.269. A failed download is a notice
   (`prepare-release-baselines.sh:47`), and publication continues.
5. **The state range is a test record.** `assemble-release-compatibility.py:58`
   sets `stateReadMin` to the lowest state version among the tested sources.
   Every executable embeds all sixteen migrations (`db/migrations`,
   `internal/storage/store.go:407`) and applies whatever is pending. "State
   version 13 is outside 15..16" means nobody tested it.
6. **The search stops at one intermediate.** `release.Client.Bridge`
   (`bridge.go:44`) looks for a single stepping stone among the 32 newest
   releases within ten seconds and only suggests a command.
7. **The coordinator policy cannot meet the goal.** It fails a target whose
   build the pinned release does not allow (`prepare`,
   `internal/update/coordinator.go:319`) and never looks for a chain. None of
   the fleet's builds contains it, and the fleet was never enrolled. A member
   that stays offline keeps the run unsettled, and `AcceptCheckedRelease`
   (`automatic_store.go:307`) starts no newer release until the current run
   settles. `consentFor` (`automatic.go:172`) binds each recipient to the exact
   membership digest and epoch, so adding a machine voids every consent. A
   failure is a sticky stop that a person must retry or acknowledge.
8. **The helper lags and nothing notices.** The PC's daemon is v0.1.249. Its
   helper receipt and `current` link name the v0.1.196 executable.
   `helperUpgradeAllowed` (`internal/updateinstall/helper.go:336`) refuses
   promotion when the running helper's own state version is outside the
   committed manifest's read range, so a helper outside that range can never
   be promoted. How the PC's helper first fell behind is not established; unit
   5 diagnoses it. The PC's helper unit is enabled and has been inactive since
   the boot on 2026-11-11. The automatic path checks only the receipt file
   (`RequireAutomaticHelper`, `engine.go:390`).
9. **Rollback after a migration is unproven.** Receipts certify that the
   candidate opened retained state. `restore` restarts the previous executable
   against a database the candidate already migrated, and no test runs that.
10. **Publication never exercises the installed path.**
    `prove-release-transition.sh` starts daemons by hand. The helper, the
    journal and the service manager are covered by Go tests with fake services
    and by an operator-run VM script (`scripts/check-updates-vm.md`).
11. **Upgrade breakage shows up after merge.** Transition proofs run only in
    the publisher. A pull request that breaks an upgrade passes CI.
12. **Installed readers are strict.** Every installed build decodes the
    manifest with unknown fields disallowed and requires exactly three
    artifacts (`client.go:253`, `release.go:205`). Receipts and the journal are
    decoded the same way (`bridge.go:207`,
    `internal/updateinstall/storage.go:161`). Adding a field to any of them
    locks older builds out.
13. **Releases are unsigned.** The approved constraint says signed releases.
    What exists is a fixed HTTPS origin and hashes published at that origin.

Outside the graph: v0.1.1 through v0.1.38 predate manifests, fourteen reserved
drafts were never published, and four tags have no release. No fleet machine
runs any of them.

## Principal decisions

1. **Each host updates itself.** A host updater inside the daemon plans and
   installs releases for its own machine with its own authority. It needs no
   coordinator, saved fleet or enrollment. This fits invariant 6.
2. **Releases are signed by the release workflow.** GitHub's artifact
   attestation signs one metadata file per release. Mesh verifies it against a
   pinned certificate authority and the exact workflow identity. No owner
   credential is involved.
3. **Discovery is a stable, paged index.** A tiny root says which format epoch
   is current. Signed, digest-linked pages list the anchors. A reader works
   forward from its own position in bounded passes with a saved cursor, and it
   never decodes a document from a format newer than its own.
4. **Direct reach is a release gate.** Each release proves transitions from
   every release since the newest anchor and from a window of anchors that
   always covers at least sixteen anchors and at least thirty days. Publication
   fails when any published release lacks a chain of verified receipts.
5. **Rollback is proven at every migration boundary** before publication, and
   stored as its own proof beside the unchanged forward receipt.
6. **Fleet order comes from durable evidence.** A host records what its
   predecessors actually committed and installs the newest release that all of
   them have run or passed. An unavailable predecessor holds the host and is
   reported. Nothing expires into permission.
7. **Adoption is a product operation.** A verified executable of the new
   release becomes the helper on each existing machine, seeds the host policy,
   and drives the old daemon through its own signed local controls. The fleet
   operation starts once, deploys over configured system SSH, orders the
   machines by evidence, notifies managing, and adopts the PC last.
8. **One mechanism.** The consent-bound coordinator policy is removed after
   the fleet runs on the host updater. Manual `mesh update` operations stay.

## Design

### Signed releases

**What is signed.** Each release gains `mesh-update.json`, the update metadata.
It carries the SHA-256 of the exact `mesh-release.json` bytes, the release's
position in the anchor index, its direct sources, the rollback proof digests,
the newest anchor page, and the trust version. `mesh-release.json` keeps its
schema 1 layout byte for byte. The signature covers the metadata, and the
metadata binds everything else by digest: the manifest, through it every
archive, executable and forward receipt, and beside it every rollback proof and
index page.

```go
// internal/release/metadata.go. Asset mesh-update.json, at most 64 KiB.
type UpdateMetadata struct {
	Schema      int            `json:"schema"`
	Epoch       uint32         `json:"epoch"`
	Trust       int            `json:"trust"`    // trust root version that verifies this release
	Version     string         `json:"version"`
	Manifest    string         `json:"manifest"` // SHA-256 of the mesh-release.json bytes
	PublishedAt time.Time      `json:"publishedAt"`
	Position    Position       `json:"position"`
	Direct      DirectSources  `json:"direct"`
	Rollback    []RollbackRef  `json:"rollback"`
	Page        AssetRef       `json:"page"` // prior published anchors; excludes this release
}

type Position struct {
	Anchor  bool   `json:"anchor"`
	Ordinal uint64 `json:"ordinal"` // newest anchor at or before this release
}

type DirectSources struct {
	AnchorsFrom uint64   `json:"anchorsFrom"` // lowest anchor ordinal proved directly
	Since       []string `json:"since"`       // every release after the newest anchor
}

type RollbackRef struct {
	Forward  string `json:"forward"`  // SHA-256 of the schema 1 forward receipt
	Rollback string `json:"rollback"` // SHA-256 of the rollback proof asset
}

type AssetRef struct {
	Tag    string `json:"tag"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}
```

**How it is signed.** The publisher runs GitHub's `actions/attest@v4`, pinned
by commit, with `mesh-update.json` and the three archives as subjects. It needs
the workflow permissions `id-token: write`, `attestations: write` and
`artifact-metadata: write`, and no secret. For a public repository the action
signs through the Sigstore public good instance: a short-lived certificate
from the Fulcio certificate authority that names the signing workflow, and an
entry in the public transparency log. The publisher preserves the action's
complete original bundle in a versioned wrapper, `mesh-update.sig.json`:

```go
// internal/release/signature.go. Asset mesh-update.sig.json, at most 256 KiB.
type SignatureEnvelope struct {
	Schema     int         `json:"schema"`
	Signatures []Signature `json:"signatures"`
}

type Signature struct {
	Kind   string          `json:"kind"`   // "github-attestation"
	Bundle json.RawMessage `json:"bundle"` // complete original bundle, at most 120 KiB
}
```

**What Mesh verifies.** `release.VerifySignature(root TrustRoot, metadata,
envelope []byte)` uses the maintained Sigstore Go verifier. Reject an envelope
larger than 256 KiB, more than two entries or a raw bundle larger than 120 KiB
before parsing bundle contents. Require one supported entry for which all of
this holds:

- the leaf certificate chains to a certificate authority in the pinned trust
  material, with a verified observer timestamp inside its validity window;
- transparency-log evidence and signed certificate timestamps verify against
  the authenticated trusted material;
- the certificate's extensions equal the pinned identity: the GitHub Actions
  token issuer, the signing workflow
  `ShaulLavo/mesh/.github/workflows/release.yml@refs/heads/main`, the numeric
  repository and owner identifiers, the source ref `refs/heads/main`, and a
  GitHub-hosted runner;
- the signature verifies over the payload with the certificate's key;
- the payload lists a subject whose SHA-256 equals the metadata bytes.

Entries of an unknown kind are skipped, so a later release can add a second
signer without breaking older readers. An envelope with no valid pinned entry
is refused. Keep complete evidence for offline verification after the
short-lived certificate expires. The official CLI also verifies the bundle
at publication and at adoption start.

```go
// internal/release/trust. Embedded data, versioned.
type TrustRoot struct {
	Version       int
	BootstrapRoot []byte // pinned Sigstore TUF root metadata
	Trusted       []byte // complete material, including log and timestamp keys
	Identity      SignerIdentity
}
```

The embedded material includes certificate authorities, log keys and timestamp
keys. The Sigstore library's TUF client starts from the pinned bootstrap root
and authenticates successive root rotations. Persist those roots and refreshed
trusted material, and retain historical material needed by archived waypoints.
Trust refresh is bounded per pass and resumes durably. An expired bootstrap
root must still authenticate its successive roots under TUF's root continuity
rules; current timestamp and snapshot expiry checks remain enforced.

**Where verification happens.**

- *Publication.* After signing, the publisher runs `gh attestation verify` for
  the metadata and each archive with `--repo ShaulLavo/mesh`,
  `--signer-workflow`, `--source-ref refs/heads/main` and
  `--deny-self-hosted-runners`. It then runs the candidate's own
  `VerifySignature`, then uses each updater-capable source reader permitted
  to take the direct unattended edge. Historical unsigned sources have no
  signature verifier. Prove their forward and rollback paths and bind their
  immutable manifest and archive digests through newly signed metadata. Any
  required verification or signing failure leaves the draft unpublished.
- *Host updater.* Every hop verifies the metadata signature before any manifest
  is trusted, then checks the manifest bytes against the metadata. The existing
  transaction re-reads the manifest and compares digests (`accept`,
  `engine.go:93`), so the helper installs exactly the signed manifest.
- *Adoption.* The adoption supervisor verifies the pinned metadata, signature,
  manifest and its own executable digest before it sends anything to the old
  daemon. The old CLI's version-only selection is never used.

**Rotation.** TUF authenticates key and trusted-material rotation from the
installed bootstrap root. A later arbitrary signing key does not replace that
chain. Incompatible reader or bundle formats still use an epoch waypoint.
Prove an old reader to its compatible authenticated waypoint, then the
waypoint reader to the next target. Test the original installed reader after
certificate expiry, key rotation and bootstrap-root expiry. No new owner
signing credential is needed for the GitHub keyless workflow.

**Limits.** A signature proves who published the content. It does not prove
freshness: an attacker who controls the origin can still serve an old, validly
signed release. `mesh update fleet` shows a host that stays behind its peers.

### Stable discovery

A host finds its next hop from three kinds of document. Only the first is
fetched from `latest`, and it is small enough to stay the same forever.

**The root.** Every release carries `mesh-update-root.json`, at most 2 KiB. Its
shape never changes.

```go
// internal/release/discovery.go
type DiscoveryRoot struct {
	Format   int       `json:"format"` // always 1
	Epoch    uint32    `json:"epoch"`  // format epoch of the release that carries it
	Head     AssetRef  `json:"head"`   // newest mesh-update.json of that epoch
	Previous *AssetRef `json:"previous,omitempty"` // root carried by the last release of the previous epoch
}
```

The root is a location hint and grants nothing. Everything it leads to is
verified by signature or by a digest chain that ends in one.

**Epochs.** A format epoch covers one set of formats a reader can decode.
An incompatible change to the manifest, metadata, pages, journal, platform set
or trust-update reader starts a new epoch. Ordinary authenticated TUF key
rotation stays within the same epoch.
The last release of an epoch is the waypoint: it is an anchor, it is written
and signed in the old epoch's formats, and its executable reads the new ones.
Its root is the final root of its epoch and never changes afterwards.

A reader compares the root's epoch with its own. If they are equal it uses
`Head`. If the root is newer it follows `Previous` until it finds its own
epoch, whose head is the waypoint's metadata. It never decodes a manifest,
metadata file or page from a newer epoch. After it installs the waypoint, the
new executable repeats the process one epoch later.

**Anchor pages.** Anchors are numbered from zero. A page holds at most 64
entries, at most 64 KiB, and pages never span an epoch. Tags and versions are
ASCII with at most 64 bytes, asset names at most 128 bytes, and digests exactly
64 hexadecimal characters. A page has at most 64 back references. Producer and
decoder enforce actual serialized byte bounds. These are per-document bounds
with no total-history cap.

```go
type AnchorPage struct {
	Schema  int           `json:"schema"`
	Epoch   uint32        `json:"epoch"`
	Page    uint64        `json:"page"`
	Entries []AnchorEntry `json:"entries"` // ascending ordinals
	Back    []PageRef     `json:"back"`    // pages 1, 2, 4, 8 ... before this one
}

type AnchorEntry struct {
	Ordinal     uint64    `json:"ordinal"`
	Version     string    `json:"version"`
	Metadata    string    `json:"metadata"`           // SHA-256 of its mesh-update.json
	Manifest    string    `json:"manifest,omitempty"` // seeds published before signing: manifest bytes
	AnchorsFrom uint64    `json:"anchorsFrom"`
	PublishedAt time.Time `json:"publishedAt"`
}

type PageRef struct {
	Page uint64   `json:"page"`
	Ref  AssetRef `json:"ref"`
}
```

A release's page contains only already-published anchors. Admit the current
target anchor from its signed head metadata, then add it to the next release's
page. This keeps the metadata/page digest graph acyclic. Full pages are
immutable; new partial pages contain prior anchors. Signed metadata names
each page by digest, and pages name earlier pages by digest. The back
references double in distance, so any
page among P is reached in about log2(P) fetches. No single asset grows with
history: 2,000 anchors are 32 pages.

**Bounds.** A discovery pass reads at most eight discovery documents and four
manifests or proofs, within 30 seconds. There is no limit on passes, hops or
total fetches. The cursor is saved in `updates/host-discovery.json`:

```go
// internal/update/host_discovery.go
type DiscoveryCursor struct {
	Epoch  uint32             `json:"epoch"`
	Root   *AssetRef          `json:"root,omitempty"` // next root during epoch traversal
	Head   AssetRef           `json:"head"`
	Pages  map[uint64]AssetRef `json:"pages"`           // page locations learned so far, newest 256
	Search *RouteSearch       `json:"search,omitempty"` // open binary search: from, low, high
	Next   *PlannedRelease    `json:"next,omitempty"`   // verified next hop
}
```

Verified pages are cached by digest. On a head change, retain only locations
proven full, refresh the partial-page reference, and reset head-dependent
searches. Persist epoch-root traversal too, so crossing more than eight epochs
continues next pass without starting over.

### Routes

`internal/release/route.go` holds the search. It is a pure function over a
document source and a cursor, and it returns either a verified next hop or an
updated cursor.

```go
func NextHop(ctx context.Context, from Build, at Position, target UpdateMetadata, cursor RouteCursor, read DocumentSource, budget PassBudget) (RouteStep, error)
```

The anchor windows only move forward, so `AnchorsFrom` never decreases from
one anchor to the next. That makes the search exact:

1. If the target lists the build in `Since`, or the build is an anchor at or
   above the target's `AnchorsFrom`, the target is the next hop.
2. A release that is not an anchor goes to the first anchor after it. That
   anchor proved every release since the anchor before it.
3. From anchor m, the next hop is the farthest anchor n, no later than the
   target's own anchor, whose `AnchorsFrom` is at most m. A binary search over
   the ordinals finds it, reading entries through the page references.
4. Repeat from the new position after the hop commits.

The index only guides. Each hop is admitted by the verified signature, by
`Manifest.Allows` on the exact executing digest, by the forward receipt, and
for automatic hops by the rollback proof. A failed edge is excluded from step
3, which then takes the next farthest anchor.

Worked cases that become tests: with every anchor proving the 24 before it,
anchor 0 reaches target 52 as 0, 24, 48, 52, although none of the newest
sixteen anchors admits 0; a 300-anchor distance takes thirteen hops and
completes; a 2,000-anchor index resolves any position in a few passes; and a
reader from the activation release facing a newer epoch installs the waypoint
without decoding anything newer.

`mesh update --check` uses the same search and prints the hops it has verified
so far. `Client.Bridge` and its 32-entry history page are deleted once the
preview uses it.

### Supported releases, anchors and the release gate

`scripts/upgrade-policy.json` holds the numbers and the seed list.

- **Anchor.** A candidate is an anchor when the previous anchor was published
  in an earlier ISO week (UTC), or when sixteen releases have been published
  since the previous anchor. A waypoint is always an anchor. The decision is
  written into the release's metadata and never changes.
- **Anchor window.** A candidate proves a direct transition from every anchor
  in a window that ends at the newest anchor. The window is the smallest one
  that holds at least sixteen anchors and reaches an anchor published at least
  thirty days before the candidate. Both floors are computed, so a faster
  release pace widens the window by itself and no publication fails for age.
- **Direct sources.** The window's anchors, plus every release after the
  newest anchor, on all three platforms.
- **Supported.** Every updater-capable release retains an unattended path,
  however old it becomes. Releases in the recent window need at most two
  hops. Pre-updater historical receipts certify forward transitions only;
  they do not certify rollback or authorize unattended adoption. The actual
  fleet daemon builds v0.1.249, v0.1.255 and v0.1.257 are required adoption
  sources with full rollback proof.
- **Reachable.** Every other published release keeps a longer chain through
  older anchors.
- **Seeds.** History before the policy becomes the first anchors. Unit 3's tool
  walks back from the first policy release and picks a ladder of published
  releases such that every release between two seeds has a verified receipt
  edge to the later seed. It stops when the ladder has sixteen seeds and its
  oldest was published thirty days before the candidate, then adds v0.1.249,
  v0.1.255 and v0.1.257 if the ladder missed them. The unit runs the forward
  proof from every seed on native runners and commits the list that passed. A
  seed that fails is fixed in the candidate or the proof harness, where
  compatibility code is allowed.

**Edges are verified receipts.** `internal/release/cmd/routes` is the
publisher's tool, written in Go so it runs the same verifier as a host. An
edge from source to target on a platform exists only when all of this holds:
the target's manifest bytes match their recorded digest; the manifest lists a
transition from the source's executable digest to the target's; the receipt
asset named by `proof` downloads and hashes to that name; the receipt passes
`VerifyTransitionReceipt` for that transition and the target's compatibility;
and the source's state and worker versions fall inside the receipt's tested
range. Receipts published before v0.1.232 are checked by a second, named
function for their own contract. An edge of either kind is forward only. An
automatic hop also needs the rollback proof named by the signed metadata.

The real v0.1.196 daemon cannot read state 13 after migration 13 removes
`services.public_name`; it also cannot read final state 16. Retain that rejected
edge as a negative proof fixture, never a direct unattended edge. The v0.1.196
fleet helper is a separate pairing test because it does not own the daemon
database. Atomic batching cannot repair unreadable final state.

The publisher refuses to publish when:

1. a direct source lacks a verified forward receipt on any platform, or an
   updater-capable or fleet-seed source lacks a rollback proof;
2. a source could not be acquired, after bounded retries;
3. any published release with a manifest has no chain of verified edges to the
   candidate on any platform, or a supported release needs more than two hops;
4. the candidate or adopter cannot verify the signed target, or an
   updater-capable reader cannot verify its permitted unattended edge;
   historical unsigned seeds need forward/rollback proof and signed digest
   binding, and epoch crossings use compatible authenticated waypoints;
5. any retained asset is missing or changed;
6. the manifest exceeds 512 KiB or does not decode with the frozen schema 1
   reader.

**Retention.** Every release named by an anchor entry or reached from one keeps
its manifest, metadata, signature, receipts, rollback proofs, archives and
pages. No release script deletes or replaces an asset of a published release.
For rule 5 the tool lists every release through the paginated API and compares
each retained asset's name and SHA-256 with the index. The release API reports
a `digest` per asset (confirmed on v0.1.306), so the comparison downloads
nothing. An asset the API reports no digest for is downloaded and hashed once.

The job summary prints the window's oldest anchor, its date, the source count
and the longest chain.

### Host updater

New files in `internal/update`: `host_updater.go` (the step), `host_policy.go`
(policy and fence), `host_state.go` (state, failed edges, ledger),
`host_discovery.go` (cursor) and `host_order.go` (predecessor evidence).
`newUpdateController` (`internal/daemon/update.go:37`) builds the updater
beside the coordinator, and `internal/daemon/app.go:584` runs it once the
listeners are ready.

```go
// updates/host-policy.json, private like automatic.json (readAuthority).
type HostPolicy struct {
	Schema   int      `json:"schema"`
	Revision uint64   `json:"revision"` // increases with every local change
	Mode     HostMode `json:"mode"`     // automatic | manual
	After    []string `json:"after,omitempty"` // predecessors by host identity
	Soak     Duration `json:"soak,omitempty"`
}

// updates/host-update.json
type HostUpdate struct {
	Schema      int             `json:"schema"`
	Phase       HostPhase       `json:"phase"` // idle | discovering | waiting | installing | attention
	Target      *PlannedRelease `json:"target,omitempty"`
	Next        *PlannedRelease `json:"next,omitempty"`
	Owned       *OwnedRequest   `json:"owned,omitempty"`
	Problem     *HostProblem    `json:"problem,omitempty"`
	NextAttempt time.Time       `json:"nextAttempt,omitempty"`
}

type PlannedRelease struct {
	Version  string `json:"version"`
	Metadata string `json:"metadata"`
	Manifest string `json:"manifest"`
	Binary   string `json:"binary"` // executable digest for this platform
}

// Binds one journal request to the policy revision that started it.
type OwnedRequest struct {
	Request        string `json:"request"` // updateinstall.Request.ID
	Generation     uint64 `json:"generation"`
	PolicyRevision uint64 `json:"policyRevision"`
	Edge           Edge   `json:"edge"`
}

type Edge struct {
	From     string           `json:"from"` // executable digests
	To       string           `json:"to"`
	Platform release.Platform `json:"platform"`
}

// updates/host-failed-edges.json. One entry per failed activation.
type FailedEdge struct {
	Edge    Edge                `json:"edge"`
	Version string              `json:"version"`
	Phase   updateinstall.Phase `json:"phase"`
	Problem string              `json:"problem"`
	At      time.Time           `json:"at"`
}

// updates/host-ledger.json. One entry per committed installation.
type LedgerEntry struct {
	Seq              uint64    `json:"seq"`
	Request          string    `json:"request"`
	Generation       uint64    `json:"generation"`
	Version          string    `json:"version"`
	Metadata         string    `json:"metadata,omitempty"`
	Binary           string    `json:"binary"`
	From             string    `json:"from"`
	Origin           string    `json:"origin"` // host | manual | adoption
	CommittedAt      time.Time `json:"committedAt"`
	Workers          int       `json:"workers"`
	WorkersPreserved int       `json:"workersPreserved"`
}
```

`HostUpdate` has one owned request at most and only in phase `installing`. It
holds no lists. Failed edges and the ledger are their own files with their own
retention rules.

**The step.** It is idempotent, runs every 30 seconds on local reads, and
touches the network only when a check is due or a catch-up is in progress.

1. Reconcile first, in any mode. Match journal and ownership by request ID and
   generation. Append a committed ledger entry idempotently by that pair
   before clearing `Owned`; a verified rollback adds a failed edge, and a
   failed rollback sets `attention`. A crash between append and ownership
   clearing must not duplicate a commit. Ownership saved before any journal
   exists is rechecked under the authority lock against policy and current
   build, then resumes the identical request or is cleared without a grant.
2. Read the policy. Manual mode stops here. A missing file means the default
   below. A malformed or unreadable file sets `attention` and stops.
3. Yield while another operation owns an unfinished journal, a recipient
   consent of the coordinator policy is enabled, or durable adoption is
   unfinished. Adoption includes the helper's confirmation phase. Its first
   automatic hop cannot race that confirmation.
4. Run one discovery pass.
5. Choose the target: the epoch head, limited by the fleet order below.
6. Stop when the executing digest equals the target's artifact or the
   executing version is newer.
7. Check the installation: kind, service preflight, required mount, free
   space, and a current, running helper.
8. Take the next verified hop from the route search, excluding failed edges,
   and check every live worker against it.
9. Persist `OwnedRequest` and its policy revision under the authority lock
   before the first stage acceptance, then stage and grant under the fence.

The daemon exits during activation. The next daemon starts at step 1.

**Authority.** The updater calls the engine in-process, as `mesh update --local`
does through the daemon today. A host's own identity is always an update
administrator on itself, and remote update controls keep requiring an enrolled
administrator.

**The fence.** `Engine.authorize` (`engine.go:355`) commits a request with no
coordinator reference at once, so the coordinator's consent check does not
protect the updater's requests. The change: `authorize` always calls the
configured acceptance when one is set. The daemon installs
`Store.AuthorizeHostAcceptance` (`host_policy.go`), which wraps the existing
`Store.AuthorizeAcceptance` (`automatic_consent.go:107`). Host-generated
stage, retry and grant calls carry an explicit in-process host origin,
including requests with nil `Automatic`; origin is not inferred from whether
an ownership file exists. A host call with missing or malformed ownership
fails closed and cannot fall through to manual acceptance. The authorizer
takes the authority lock (`lockContext`), matches the request ID, generation
and policy revision, requires automatic mode, and commits inside that lock.
No journal format change is needed. `mesh update policy set` and `off` take the same lock and
increase the revision. So `policy off` returns only after any commit in
progress, and every later stage, retry or grant of that request is refused.
Work already granted finishes in the helper, and step 1 records its result in
manual mode too. The journal format does not change. Only a local Unix-socket
caller may change the policy.

**Default.** With no policy file the mode is automatic when the executable is a
stable release build on a Mesh-managed path with a daemon service. Development
and modified builds are always manual.

**Schedule.** In automatic mode the updater fetches the root every 30 minutes
plus up to five minutes of jitter, at daemon start, and when the step timer
shows a clock jump, which is how a laptop notices a release on waking. The
existing `conditionalTransport` keeps an unchanged answer cheap. Releases
published between two checks collapse into one update. The reminder in the
picker keeps its own six-hour check.

### One hop

A hop is today's transaction: `accept` re-reads the manifest from the origin
and compares digests, `preflight` checks the executing digest against the
installed file and every worker's protocol, and the helper activates and
verifies.

One thing changes. The helper must be at least as new as the installed daemon
and must be running before a hop starts.

- After a commit, promotion requires only that the new helper image answers
  `update-helper --check-journal`. The test on the old helper's state version
  in `helperUpgradeAllowed` goes away. The helper owns the journal and does not
  own the database.
- `updateinstall.PromoteHelper(ctx, cfg, source, digest)` promotes a verified
  executable to be the helper while the journal is in a finished phase. It is
  the existing preparation, restart, process binding and restoration path
  (`prepareHelper`, `helper.go:48`; `promoteRecoveryHelper`,
  `helper_recovery.go:160`) opened to committed journals.
- Before a hop, the updater compares the helper receipt and the running helper
  process (`updatebootstrap.HelperProcessProbe`) with the installed daemon. A
  stale or stopped helper is promoted or started. If that fails, the host
  reports `attention: update helper` and starts no hop.

External promotion and readiness calls require an injected running-process
probe, supplied by `updatebootstrap.HelperProcessProbe`. The current
`update.EnsureHelper` caller uses the committed journal's exact manifest;
it does not need another network lookup. First-install initialization retains
its existing path, and unfinished transactions keep ownership. The helper's
own `UpgradeHelper` path never waits for its own replacement.

So a hop from release S is run by S's daemon and S's helper, and an adoption
hop is run by the old daemon and the pinned release's helper. CI tests both
pairs.

### State migration and rollback

Rules for schema changes, enforced by the proofs:

- A shipped migration is never edited, squashed or removed. A candidate must
  open the state of every direct source.
- Every state a failed upgrade can leave behind must remain usable by the
  source executable. A change that an older source cannot live with waits
  until that source is routed through a waypoint, or the migrations it spans
  become one atomic batch.
- Rollback restores the executable and never restores a database snapshot. T26
  decided that, and it holds.

**Forward receipts stay as they are.** Schema 1, the exact current fields, the
manifest's `proof` naming them. The actual v0.1.249 receipt reader and the
actual v0.1.196 journal reader run against generated release assets in unit 8.

**Rollback proofs are separate assets**, named by their digest and listed in
the signed metadata:

```go
// internal/release/rollback.go. Asset <sha256>.rollback.json.
type RollbackProof struct {
	Schema     int                `json:"schema"`
	Platform   Platform           `json:"platform"`
	FromDigest string             `json:"fromDigest"`
	ToDigest   string             `json:"toDigest"`
	Forward    string             `json:"forward"` // SHA-256 of the forward receipt
	Boundaries []RollbackBoundary `json:"boundaries"`
	Workers    []string           `json:"workers"` // executable digests of the worker fixtures kept alive
}

type RollbackBoundary struct {
	State             int  `json:"state"` // database version the source was started on
	CandidateWrote    bool `json:"candidateWrote"`
	SourceStarted     bool `json:"sourceStarted"`
	SessionIO         bool `json:"sessionIO"`
	RecoveryPreserved bool `json:"recoveryPreserved"`
}
```

Goose commits each migration separately, so a failed upgrade can stop at any
version between the source's and the candidate's. The proof covers each one.
`internal/storage/cmd/boundary` supplies fast copied-state fixtures using the
candidate's exact migrations. The required proof also interrupts or fails the
actual candidate immediately before and after each migration commit, then
runs real helper executable rollback and source startup on the resulting
state. At every reachable boundary, require retained session I/O, unchanged
worker and shell identity, and unchanged recovery records. Exercise candidate
writes and persistent configuration changes wherever they can occur before a
later failure, including service routes and machine naming. Advisory client
storage writes are tested separately and identified as such.
`VerifyRollbackProof` requires every boundary in the range, its actual
candidate/helper evidence, and a matching forward receipt. A copied SQL
fixture alone does not certify a failed candidate's resulting state.

A boundary that fails means the release cannot publish. A failing seed is fixed
by a compatible migration, or by an atomic batch, before rollout.

### Sessions and services across hops

- Workers own their PTYs and keep their original executable. `verifyHealth`
  fails a hop when any worker's PID or shell PID changed, and that failure
  rolls the hop back.
- The hard link to each replaced executable stays while a live worker runs it
  (`internal/updateinstall/prune.go:20`).
- A worker can outlive many daemon releases. `scripts/upgrade-policy.json`
  lists every worker implementation that can still be running, starting with
  the seeds' builds. An entry is added whenever the worker protocol or the
  worker's files change, and none is removed. Every release proves it serves
  each listed implementation: unit 8 keeps one worker from each alive through
  several real daemon and helper hops and a rollback at every hop. An
  incompatible candidate fails publication. A live session never blocks an
  update and is never ended to allow one.
- New sessions stay gated during validation (`internal/updategate`).
- The daemon's own listeners restart with it. A process inside a session, such
  as the PC's model proxy, keeps running and keeps its sockets. Mesh routes to
  it return when the new daemon starts. Unit 8 keeps a terminal attached and a
  service stream open across activation, forced rollback and helper
  replacement, and requires reconnection and a bounded route gap.

### Failure handling

| What happened | What the updater does |
| --- | --- |
| Check, download, space, mount, lock or preflight failed before activation | Retry with backoff from 15 minutes to six hours. The daemon was never stopped. |
| Activation failed and rollback verified | Record a `FailedEdge` for the exact source and target executables. The route search excludes it for every later target and takes another verified hop, or waits for one. |
| Rollback failed | `attention`. No further hops. The helper keeps both executables and the journal. Recovery follows `.internal/helper-recovery.md`. |
| A manual operation owns the journal | Wait, and resume when it finishes. |
| A predecessor is behind, unavailable or refusing | Hold at the release the fleet order allows, and report which peer and why. |

A failed edge is kept while the host still runs its source executable. A newer
final target does not clear it. It is dropped when the host commits any other
release, because the host never returns to an older executable, or when a
local `mesh update --local --yes` names that hop again. Failed edges survive
restarts and are not limited in number.

### Fleet order

Order is decided by what predecessors actually committed, recorded durably on
both sides.

**The record.** Each host keeps its ledger: one entry per committed
installation. A predecessor answers a new read-only control, `update.evidence`
(`internal/protocol/control.go`), over the ordinary authenticated transport. It
needs device access and no update-administrator grant.

```go
type EvidenceRequest struct {
	Settled  string `json:"settled,omitempty"` // the reader's own committed version
	AfterSeq uint64 `json:"afterSeq"`
	Limit    int    `json:"limit"` // at most 64
}

type EvidenceReply struct {
	Mode      string        `json:"mode"`
	Phase     string        `json:"phase"`
	Entries   []LedgerEntry `json:"entries"`
	More      bool          `json:"more"`
	Failed    []FailedEdge  `json:"failed,omitempty"`
	Attention string        `json:"attention,omitempty"`
}
```

A reader that sends `Settled` is registered as a dependent with that version.
The predecessor keeps every ledger entry newer than the lowest version among
its dependents, and always its newest. There is no count limit, so a burst of
releases cannot push an entry out before a slower dependent has read it. A
dependent that stops reading is shown in the predecessor's status with the
number of entries it holds back. It is released when its own policy drops the
predecessor, or locally with `mesh update policy forget`.

Initial-adoption milestones have separate retention in the durable adoption
record and are returned with evidence until that fleet operation settles.
Bind adoption and later unattended success to the operation ID and exact
pinned adoption digest. Ordinary ledger pruning cannot discard them before a
dependent's first read.

The dependent saves what it reads in `updates/host-peers.json`: per
predecessor, the entries newer than its own version, the failed edges, and the
state of the last read with its time.

**The rule.** For each predecessor, take its newest ledger entry committed at
least `Soak` ago. The host's limit is the lowest of those versions across all
its predecessors. The host installs the limit when it is newer than its own
release.

- The limit was committed exactly by one predecessor, and every other
  predecessor has committed and soaked on that release or a newer one. A
  canary that is already on a newer release counts for the limit. It does not
  let the host skip ahead to a release only that canary has run.
- Predecessors do not need a release in common. One on A, C, E and another on
  B, D, F give the limit E once their newest entries have soaked.
- A predecessor whose hop failed stays on its old release, so the limit stays
  at or below it. Its failed edge also holds the same hop on a dependent of
  the same platform.
- A predecessor in `attention` holds its dependents at their current release.

**Unavailable predecessors.** The read distinguishes four results, and none of
them expires into permission:

| Result | Meaning | Effect |
| --- | --- | --- |
| offline | No connection | Keep the saved evidence. Advance only as far as it allows. |
| refused | The peer denied this identity, or presented another identity | Hold. Report the peer and the approval it needs. |
| unsupported | The peer answers but does not know the control | Hold. The peer is not adopted yet. |
| invalid | The reply fails validation | Hold and report. |

A host that wakes reads its predecessors before it chooses a target. If a
predecessor committed a release ten minutes ago and the one before it three
hours ago, a two-hour soak selects the earlier one now and the later one after
it has soaked.

**The owner's fleet.** The Pi and the VPS have no predecessors. They are always
on, so they are the canaries. The Mac follows both with a 30-minute soak. The
PC follows both with a two-hour soak. The Mac is required for initial PC
adoption, but is not an ordinary PC-policy predecessor; an offline Mac does
not block later PC updates. Removing a dead canary from a policy is
a local, explicit change. A new machine has no predecessors.

### Visibility for managing

The host info reply gains an optional summary. `DecodeControl`
(`control.go:376`) tolerates unknown fields, so peers on older builds keep
reading host info.

```go
// protocol.HostInfo.Update
type HostUpdateSummary struct {
	Mode         string       `json:"mode"`
	Installation string       `json:"installation"` // managed | package | client | development
	Phase        string       `json:"phase"`
	Target       string       `json:"target,omitempty"`
	Waiting      string       `json:"waiting,omitempty"` // peer and reason
	Problem      *HostProblem `json:"problem,omitempty"`
	Last         *LedgerEntry `json:"last,omitempty"`
}
```

`mesh update fleet [--json]` reads the host info of every member of the local
saved fleet and prints one row each: version, installation kind, mode, phase,
target, releases behind, what it waits for, last committed update and current
problem. It exits 0 when every member is current and automatic, 2 when a
member is pending inside its soak, and 1 when a member has a problem, is not
automatic, has been behind for more than six hours past its soak, or has been
unreachable for a day. The managing chat's hourly check runs it on the PC. The
exit status and the `problems` array are how managing is told.

The dashboard's update attention area (plan 14 unit 5) reads the same summary
in place of the coordinator policy record.

### Offline, sleep and reboot

- **Offline.** A failed check retries after 15 minutes. Nothing else changes.
- **Back after any absence.** Discovery resumes from the saved cursor and runs
  as many bounded passes as the distance needs. Supported releases need at
  most two hops.
- **Asleep during a hop.** The helper's journal carries the transaction. It
  continues on wake.
- **Reboot during a hop.** The helper resumes its journal after boot, as today.
  The reboot interrupts sessions, and the result reports them as interrupted
  (invariant 5). The updater state is on disk and reconciles at daemon start.
- **Daemon crash between hops.** The next start runs the same step.

### Installation kinds

`updatebootstrap.Inspect` and `ValidateInstallationPath`
(`internal/updateinstall/ownership.go:13`) classify every installation, and
the kind is reported in every status row.

| Kind | What it is | Policy |
| --- | --- | --- |
| managed | Daemon service running a Mesh-managed executable | Automatic by default. |
| package | Daemon service running an executable a package manager owns | Never counted as current or automatic. Adoption migrates it. |
| client | No daemon; a command only | Reported as client-only with its version. The reminder stays. |
| development | Modified or unversioned build | Manual. Reported as such. |

The supported installers already produce a managed daemon. `mesh daemon
install` and `mesh add` copy the executable to the managed path and write the
service (`installLocalDaemon`, `internal/cli/install.go:54`), also when the
command itself came from Homebrew. A Homebrew copy beside a managed daemon is
a second command. It is listed with its own version and its package manager's
command (`OtherCommands`, `internal/updateinstall/commands.go:30`) and never
reported as updated by Mesh.

A daemon that runs from a package-owned path is migrated by adoption: the
supervisor places the verified executable of the same release at the managed
path, rewrites only the executable path in the existing service definition,
restarts the daemon through the helper transaction with rollback, and leaves
the package's files alone. Until that succeeds the row says `package` and the
fleet command exits 1.

### New installations

`scripts/install.sh` and `mesh daemon install` install the update helper with
the daemon, and `daemon installation-check` verifies that it runs. Today the
helper appears only at the first update. A fresh machine has no policy file, so
it is automatic with no predecessors from its first daemon start.

### Downloads and cache

- **Host.** One hop's archive at a time, at most 128 MiB (`maximumArchive`),
  after the existing 128 MiB free-space and required-mount checks. Discovery
  documents are bounded per pass as above. `update.PruneCache` keeps the
  installed release and the hop in flight and runs after every commit. The
  discovery cache keeps verified pages at or after the host's own position.
  `MESH_UPDATE_CACHE_DIR` and `MESH_UPDATE_REQUIRED_MOUNT` apply unchanged.
- **Rollback images.** Unchanged: the latest previous executable, plus any
  image a live worker still runs.
- **CI.** Source executables, manifests and receipts are immutable, so the
  proof jobs cache them by tag and platform and download only new ones.

### The coordinator policy

The consent-bound policy and the host updater would compete for the same
journal. Until the policy is removed, the updater stays idle on a host with an
enabled recipient consent (`automatic-consent.json`). No fleet machine has one.
Unit 13 removes the policy after the fleet runs on the host updater. `mesh
update` fleet operations, administrator trust and configuration delivery are
untouched.

## Adopting the existing fleet

v0.1.249, v0.1.255 and v0.1.257 contain no code that installs a release
without an approval, and publishing a release cannot change that. Something
that already has authority on each machine must start the new code there. That
authority exists: software running in the daemon's own account can read the
identity key and sign local update controls as the host itself. The PC's
administrator policy holding only its own identity blocks other machines. It
does not block the PC's own account.

Adoption uses that authority as a product operation. The same operation is the
supported way to bring any existing installation into the automatic policy.

### The operation on one host

`mesh update adopt` runs from a verified executable of the pinned release that
sits outside the installed path. The work is durable in
`updates/adoption.json` and is carried by the helper service, which outlives
the daemon.

```go
// internal/update/adoption.go
type Adoption struct {
	Schema     int             `json:"schema"`
	ID         string          `json:"id"` // one identifier for the fleet operation
	Pinned     PlannedRelease  `json:"pinned"`
	Signature  string          `json:"signature"` // SHA-256 of mesh-update.sig.json
	Host       AdoptionHost    `json:"host"`
	Policy     HostPolicy      `json:"policy"`  // written before activation
	Require    []AdoptionPeer  `json:"require"` // hosts that must finish first
	Notice     *AdoptionNotice `json:"notice,omitempty"`
	Remotes    []AdoptionRemote `json:"remotes,omitempty"` // on the starting host only
	Phase      AdoptionPhase   `json:"phase"`
	Request    string          `json:"request,omitempty"`
	Generation uint64          `json:"generation,omitempty"`
	Problem    string          `json:"problem,omitempty"`
}

type AdoptionHost struct {
	ID           string              `json:"id"`
	Build        release.Build       `json:"build"`
	Installation string              `json:"installation"`
	Service      updateinstall.ServiceSpec `json:"service"`
	Helper       string              `json:"helper"`    // receipt digest before adoption
	Preserved    map[string]string   `json:"preserved"` // file name to SHA-256
}

type AdoptionPeer struct {
	ID        string `json:"id"`
	Adopted   bool   `json:"adopted"`   // ledger entry with origin adoption
	Unattended bool  `json:"unattended"` // a later ledger entry with origin host
}

type AdoptionNotice struct {
	Command     []string  `json:"command"` // run without a shell
	Lead        Duration  `json:"lead"`
	DeliveredAt time.Time `json:"deliveredAt,omitempty"`
}

type AdoptionRemote struct {
	ID      string `json:"id"`
	SSH     string `json:"ssh"` // alias in the account's SSH configuration
	State   string `json:"state"`
	Problem string `json:"problem,omitempty"`
}
```

The phases run in this order, and each is safe to repeat:

1. **Inspect.** `updatebootstrap.Inspect` (`internal/updatebootstrap/probe.go:46`)
   reads the running daemon's executable, identity, build, state version and
   workers. The supervisor adds the installation kind, the service definition,
   the journal and the helper receipt and process. The journal must be in a
   finished phase.
2. **Verify.** The pinned metadata, signature and manifest are verified with
   the executable's own trust root. The manifest must allow the observed build
   by exact digest, with a forward receipt and a rollback proof. Every live
   worker must be compatible. The executable's own digest must equal the
   manifest's artifact for this platform.
3. **Record what must not change.** SHA-256 of the identity's public half,
   `authorized_keys`, `updates/administrators.json`, the device grants, the
   daemon's service definition, and the host book, fleet and domain files.
   State paths, the cache directory and the required mount come from the
   journal's settings and the service definition and are used as they are.
4. **Seed the policy.** Write `updates/host-policy.json` with the mode,
   predecessors and soak for this host. The old daemon ignores the file. The
   new daemon reads it at its first start, so there is no window in which it
   runs with a default.
5. **Become the helper.** `PromoteHelper` makes the verified executable the
   helper image and starts it under the existing helper unit. On the PC this
   also ends the stopped v0.1.196 helper. From here the helper process carries
   the adoption: `updateHelperStep` gains `resumeAdoption` beside
   `resumePendingCoordinatorSetup`.
6. **Wait.** The helper reads `update.evidence` from each required host until
   every one shows an adoption commit for this operation and exact pinned
   digest, followed by a later unattended commit with all workers preserved.
   These milestones remain durable until the fleet operation settles,
   independently of normal ledger pruning. An unavailable, refusing or not-yet-adopted
   peer keeps the host waiting and is reported. Nothing waives it.
7. **Notify.** When a notice is configured, the helper runs its command. A
   zero exit records the delivery time. A failure is retried with backoff and
   blocks activation. After delivery the helper waits the lead time, then
   checks step 6 once more.
8. **Activate.** The helper signs `info`, `stage` and `grant` as the host's own
   identity and sends them to the old daemon over the Unix socket. The old
   daemon runs its own staging code: it finds the helper present, re-reads the
   pinned manifest from the origin, compares digests, downloads the candidate
   and writes the journal. Assert that the promoted helper receipt, link and
   actual running image remain unchanged across old staging, activation and
   rollback. The new helper then runs the activation with the
   current transaction code, verifies the new daemon and every retained
   worker, and commits or rolls back.
9. **Confirm.** After the commit the helper checks that the files from step 3
   are unchanged, that the daemon runs the pinned executable under the same
   service with the same arguments and environment, and that the seeded policy
   is in force. It idempotently writes a ledger entry with origin `adoption`
   and the operation-specific milestone, then durably marks confirmation
   finished. The host updater yields until this phase completes.

A rollback leaves the old daemon running with the new helper, which is an
accepted pair. The record becomes `rolled-back`, the edge is recorded as
failed, and the fleet operation stops at that host until a release that fixes
the cause is pinned by a new start.

What adoption never does: it sends no update control to another host, starts
no Mesh session to run commands, and never calls the legacy session bootstrap
(`Coordinator.bootstrap`, `internal/update/legacy.go`). A refused control or
evidence read is a blocker with the peer named, and stays one.

### The fleet operation

It starts once, on the PC, in the owner's account, from the verified
executable:

```text
SIDE/mesh update adopt start --release ACTIVATION --fleet ~/.config/mesh/fleet.json \
  --ssh PI=pi --ssh VPS=vps --ssh MAC=shaul-mac \
  --canary PI --canary VPS --last LOCAL \
  --notice-lead 30m --notice -- t3 thread send MANAGING_THREAD "..."
```

That one start is the bootstrap. Nobody runs an update command on any machine
afterwards, and nobody chooses or installs an intermediate release.

- The command verifies and inspects locally, writes the PC's record with all
  three other machines required and the notice attached, seeds the PC's
  policy, and promotes the PC's helper. The PC's daemon is not touched.
- The PC's helper then deploys to each remote host over the SSH alias given
  for it, using the SSH client Mesh already has (`connectSSH`,
  `internal/bootstrap/ssh.go:65`; `applySSHConfig`, `ssh_config.go:69`) with
  the account's own configuration and known hosts, and no prompts. The remote
  steps are fixed: download the platform archive from the release origin,
  compare its SHA-256 and the executable's with the values from the signed
  manifest, and run `mesh update adopt start --local` with the host's record
  on standard input. The remote executable verifies the signature again
  itself. A host that is asleep is retried with backoff. A host with no alias,
  a failing login, or an SSH account that differs from the daemon's account is
  `blocked` with that exact reason.
- The Pi and the VPS require nobody, so they activate at once. The Mac
  requires both of them. The PC requires all three.
- Each requirement needs a release published after the adoption, so that the
  host can update unattended. Mesh publishes on every merge to main, which is
  several times a day.
- When the PC's requirements hold, its helper notifies managing, waits the
  lead time, and activates the PC. No go-ahead is awaited.

`SIDE/mesh update adopt status --json` and `SIDE/mesh update fleet --json` are
read-only and work before the PC is adopted, because the side executable is
the new command.

### What the operation needs

These are the only things outside the product. Each is checked by the start
command before anything is written, and a missing one is reported as the exact
blocker.

1. **One start in the PC's account.** The lane coordinator's shell in the
   owner's account is the already-authorized execution. Before the start it
   downloads the pinned archive and verifies it with the official verifier:
   `gh attestation verify ARCHIVE --repo ShaulLavo/mesh --signer-workflow
   ShaulLavo/mesh/.github/workflows/release.yml --source-ref refs/heads/main
   --deny-self-hosted-runners`.
2. **Non-interactive system SSH from the PC's account to each daemon's
   account.** The aliases `pi`, `vps` (root) and `shaul-mac` exist in the PC's
   SSH configuration. Key authentication without a prompt is confirmed at
   start.
3. **Device access for evidence reads.** The PC must be an approved device on
   the Pi, the VPS and the Mac, and the Mac on the Pi and the VPS. The
   dashboard already relies on the first.
4. **A notice command.** `t3 thread send` exists on the PC. The managing
   thread's identifier is supplied at start.

## Implementation units

Each unit is one pull request with its own independent review. Hours are agent
hours for implementation and one review round. Replace each with the measured
time when it lands. Until unit 11 the default mode constant is manual, so
earlier releases change no behavior.

| Unit | Outcome | Depends on | Hours |
| --- | --- | --- | --- |
| 1 | Signed metadata, verifier, signing in the publisher | none | 5 |
| 2 | Discovery formats, pages and route search | 1 shared metadata types | 4.5 |
| 3 | Publisher: sources, seeds, verified graph, retention | 1, 2, 4 | 5.5 |
| 4 | Rollback proofs at every migration boundary | none | 4 |
| 5 | Helper stays current; promotion from a verified executable | none | 2.5 |
| 6 | Host updater: state, fence, failed edges, ledger | 1, 2, 5 | 6 |
| 7 | Fleet order from evidence | 6 | 4 |
| 8 | Installed proofs in CI | 3, 6, 7, 10 | 9 |
| 9 | Status, policy and fleet commands, installation kinds | 6 | 4 |
| 10 | Adoption operation | 1, 5, 6, 7 | 6.5 |
| 11 | Installer and automatic default | 8, 9 | 2 |
| 12 | Fleet adoption | 11 | 3, over several days |
| 13 | Remove the coordinator policy, write the ADR | 12 | 2.5 |

About 59 agent hours. Units 1, 4 and 5 start at once and share no files.
Unit 1 delivers its shared metadata contract first; unit 2 starts from that
commit and owns only discovery and route files. It does not duplicate types.
Unit 6 starts when 1, 2 and 5 merge, and unit 3 when 1, 2 and 4 merge. Units
7 and 9 follow 6 in parallel, then unit 10, then unit 8.

**Unit 1: signed metadata.** Files: `internal/release/metadata.go`,
`signature.go`, `internal/release/trust/` (embedded root version 1),
`internal/release/cmd/sign` (complete bundle wrapper and verify),
`.github/workflows/release.yml`, `scripts/check-packaging.sh`. Tests on a
recorded real bundle and generated certificates: a missing envelope, an
invalid signature, a certificate from another authority, another workflow,
another ref, another repository identifier and a self-hosted runner are each
refused; an altered manifest, metadata file, archive or page is refused; an
envelope with only unknown kinds is refused, and one with an unknown kind
beside a valid one verifies; a failed signing step or a failed `gh attestation
verify` leaves the draft unpublished; supported old readers verify through
authenticated TUF rotation and format waypoints, including expired bootstrap
roots; unsigned historical readers are not asked to verify signatures. Keep
observer-time, log and SCT evidence, wrapper byte bounds and unknown-kind
handling covered. The dependency buys maintained complete verification and
authenticated trust updates.

Deliver `metadata.go` and its tested shared types first: `UpdateMetadata`,
`Position`, `DirectSources`, `RollbackRef` and `AssetRef`. Unit 2 consumes that
exact commit. Before unit 3 generates route metadata, unit 1 signs the existing
release manifest and archives through the same verifier and complete bundle
wrapper. Unit 3 adds `mesh-update.json` as a signed subject. Do not publish
fabricated or incomplete index metadata to connect these phases.

**Unit 2: discovery and routes.** Files: `internal/release/discovery.go`,
`route.go`, `internal/update/host_discovery.go` and tests. Tests: the 0, 24,
48, 52 case with the newest sixteen anchors not admitting 0; a thirteen-hop
route completes; 2,000 anchors resolve within the per-pass budget and no
document exceeds its bound; a pass that hits its budget resumes from the saved
cursor without repeating fetches; a page whose digest differs is refused;
page construction contains no current-release hash cycle; a changed head
refreshes a partial page and resets head-dependent search; maximum-field
pages serialize below 64 KiB and reject invalid field bounds; a reader of
epoch 1 crossing more than eight newer epochs persists traversal, installs
the first waypoint, and never opens a newer document; a failed edge makes the
search choose the next farthest anchor.

**Unit 3: publisher.** Files: `internal/release/cmd/routes`,
`scripts/upgrade-policy.json`, `scripts/prepare-release-baselines.sh` and its
test, `scripts/assemble-release-compatibility.py`,
`internal/release/cmd/manifest`, `.github/workflows/release.yml`, `ci.yml`.
The tool selects anchors and the window, builds pages, metadata and the root,
verifies edges from receipt bytes, and checks retention. Tests: a window at
exactly thirty days and one day short; a pace of twenty releases a day widens
the window without failing; a corrupt receipt, a receipt for another
transition and a missing proof are each no edge; a source download that fails
twice and then succeeds passes, and one that keeps failing blocks publication;
a deleted or replaced retained asset blocks publication; the generated
manifest and receipts decode with the published v0.1.249 executable. The unit
also selects and commits the seed list after proving every seed. `ci.yml` gains
a path-filtered pull-request job on the three native runners that runs the
forward and rollback proofs from the previous release, the newest anchor and
the fleet seeds against a candidate built from the pull request. Measure proof
time per source and shard across matrix jobs if a platform passes ten minutes.

**Unit 4: rollback proofs.** Files: `internal/release/rollback.go`,
`internal/storage/cmd/boundary`, `scripts/prove-release-transition.sh`,
`scripts/test-release-transition.py`,
`integration/release_transition_checkpoint.sh`. Tests: a candidate whose
intermediate state the source cannot start on fails; a failure injected at
each boundary in the actual candidate followed by real helper rollback still
yields a running source with session I/O and unchanged recovery records;
candidate writes before a later failure remain readable by the source; a proof
that skips a version, or names another forward
receipt, is refused; the forward receipt stays byte-compatible with schema 1.

**Unit 5: helper currency.** Files: `internal/updateinstall/helper.go`,
`helper_probe.go`, `helper_recovery.go`, `internal/cli/update_helper.go`,
`internal/update/local.go` and
tests. Tests: a helper three state versions behind is promoted after a commit;
`PromoteHelper` on a committed journal binds the new process to its image; a
stopped helper is started before a hop; a failed start restores the prior
helper and reports; an interrupted promotion recovers;
`scripts/prove-helper-recovery.sh` still passes. Diagnose here why the PC's
unit is inactive after boot.

**Unit 6: host updater.** Files: `internal/update/host_updater.go`,
`host_policy.go`, `host_state.go` and tests, `internal/updateinstall/engine.go`
(`authorize`), `internal/daemon/update.go`, `internal/daemon/app.go`. Tests
with the real engine and a fake service: a two-hop route commits both hops
with one worker identity throughout; a daemon restart between hops resumes; a
hop that rolls back stays excluded across restarts and across more than eight
newer targets that route through it, and a verified alternative proceeds;
`policy off` and a revision change racing download, stage, retry and grant
issue no later work; manual mode records the result of work already granted;
a malformed policy or missing host ownership sets `attention` and starts
nothing; ownership saved before any journal and crashes after ledger append
recover without duplicate commits; unfinished adoption confirmation blocks
automatic hops; a failure before
activation backs off without stopping the daemon; a manual operation in the
journal makes the updater wait; a package-owned path and a development build
stay manual; an enabled recipient consent keeps it idle; an unsigned or
wrongly signed hop is never staged.

**Unit 7: fleet order.** Files: `internal/update/host_order.go` and its test,
`internal/protocol/control.go`, `internal/daemon` control handling. Tests with
three daemons: disjoint histories give the lower newest release; a burst of
twenty commits inside one soak loses no entry a dependent still needs; a host
that wakes during a soak takes the earlier soaked release, then the later one;
a canary on a newer release does not let a dependent skip to it; a failed hop
on a predecessor holds dependents below it and holds the same edge on the same
platform; offline, refused, unsupported and invalid reads are reported apart
and none releases the hold after any time; a dependent that settles lets the
predecessor prune.

**Unit 8: installed proofs.** Files: `scripts/prove-installed-catchup.sh` and
`.py`, `scripts/fixtures/fleet-baselines.json`, `.github/workflows/ci.yml`,
`release.yml`. It reuses `scripts/fixtures/published_releases.py` and `tls.py`
for a loopback release origin. The service manager is real: a systemd user
manager on Linux runners and the launchd `gui` domain on macOS runners,
confirmed before the rest is built. A platform without a usable hosted manager
runs in a disposable CI VM with the real manager. A command provider is a
supplement and does not satisfy this gate. Every scenario keeps a terminal
attached and a service stream open, and checks reconnection, unchanged worker,
shell and listener identities, the route gap and the helper process.

1. *Adoption pairs.* Install each fleet pair from published archives,
   including the PC's v0.1.249 daemon with a stopped v0.1.196 helper. Run the
   adoption operation to the candidate. Require the preserved files unchanged,
   the seeded policy in force at the first start, and the sessions intact.
   Force a failing candidate and require a verified rollback to the old
   daemon. Run the published v0.1.249 receipt reader and the v0.1.196 journal
   reader against the generated assets.
2. *Published updater.* Install the first published release that contains the
   host updater, in explicit automatic mode, and let it reach the candidate
   through an actually installed intermediate daemon and helper. Variants
   interrupt helper replacement, restart between hops, and fail the second hop
   to check rollback and the persistent failed edge.
3. *Oldest workers.* Keep one worker from each listed implementation alive
   across several real hops, with a rollback at every hop.
4. *First updater forever.* Retain that first updater release as an installed
   baseline. Its original reader must reach every candidate through retained
   releases, including a fixture epoch with an incompatible manifest that it
   reaches through a waypoint.
5. *Order.* Three installed hosts with predecessors run through a burst of
   releases, one failing canary and one sleeping host.

At publication the scenarios run against the draft's real assets and real
signature. On pull requests the candidate is built with a test trust root
under a build tag, and that build is never published. The job joins `Complete
CI gate` for pull requests that touch update, storage, migration, worker,
protocol, daemon or release-script paths.

**Unit 9: status and commands.** Files: `internal/protocol/control.go`,
`internal/daemon` host info, `internal/cli/update_policy.go`,
`internal/cli/update_fleet.go`, `internal/cli/update_bridge.go`,
`update_readiness.go`, `internal/tui/dashboard_*`, `docs/guides/updates.md`,
`docs/reference/cli.md` (generated). The policy commands are `show`, `set`,
`off` and `forget`. `mesh update --local --yes` on a host with the updater
hands the target to it. Tests: policy changes are refused
from a network peer; `mesh update fleet --json` reports current, behind,
waiting, failed, package, client and unreachable members with exit 0, 2 and 1;
an older peer without the summary reads as not adopted.

**Unit 10: adoption.** Files: `internal/update/adoption.go` and its test,
`internal/cli/update_adopt.go`, `internal/cli/update_helper.go`,
`internal/bootstrap` (an exported non-interactive runner over the existing SSH
client). Tests with a real old daemon: the supervisor signs as the host and
the old daemon stages the pinned manifest; a manifest or signature that
differs from the pin stops before any control is sent; the policy file exists
before the new daemon's first start; automatic hops wait for confirmation;
operation-specific adoption and later-unattended milestones survive ledger
pruning before a dependent's first read; the promoted helper receipt, link and
running process survive old staging, activation and rollback; identity, administrators, device grants,
service definition and state paths are unchanged; a required peer that is
offline, refusing or not adopted keeps the host waiting with that reason and
never falls back to a session; the notice command must succeed before
activation, and its lead time is honored; the last host activates only after
every required host shows an adoption commit and a later unattended one; a
package-owned daemon is migrated to the managed path with its service
arguments intact; a killed helper resumes at the same phase.

**Unit 11: defaults.** Files: `scripts/install.sh`, `scripts/install/linux.sh`,
`darwin.sh`, `internal/cli/install.go`, the default mode constant. Tests: a
fresh install runs a helper and is automatic at its first start. The release
that contains this unit is the activation release.

**Unit 12: fleet adoption.** The rollout below. No code.

**Unit 13: removal.** Delete only consent-policy-specific code in
`internal/update/automatic*.go`, `internal/cli/update_automatic.go`,
`integration/automatic_fleet_updates.sh` and its tests and docs sections.
Retain `internal/update/joined.go` and the ordinary coordinator's
installation-joining and cancellation tests: `coordinator.go:224`, `:266`,
`:389` and `:394` use those functions for manual fleet operations. Add
`docs/adr/009-each-host-updates-itself.md`. Mark plan 14's binary steps closed
and turn `/work/reports/mesh-auto-update/PLAN.md` into a pointer to this file.

## Rollout

The lane coordinator runs this. Every step records its output under
`/work/reports/mesh-auto-update/rollout/`.

**0. Inventory, read-only, any time.** For each machine: `mesh version
--json`; the phase, release and settings of `update/installation.json`; the
helper receipt digest and the helper unit's state; the executable path and
installation kind; the daemon's account; live sessions; free space. The PC is
recorded in the design evidence. The result fills `fleet-baselines.json` for
unit 8.

**1. Activation release.** It is published by normal CI, signed, with unit 8
green for all four fleet pairs.

**2. Start.** On the PC, download the activation archive, verify it with `gh
attestation verify`, extract it to a side directory, and run the start command
from the adoption section. Its preflight confirms the four needs. Record the
operation identifier.

**3. Watch.** `SIDE/mesh update adopt status --json` shows each host's phase
and what it waits for. Expected order: the Pi and the VPS adopt; the next
release reaches both unattended; the Mac adopts when it is awake; the next
release reaches it; the PC's helper notifies managing, waits the lead time,
and adopts the PC. The coordinator relays nothing by hand and runs nothing on
any machine.

**4. Check each adoption as it lands.** The daemon version, each session's
PID, the helper, the policy, and the preserved files. On the PC also the model
proxy session's PID and replies and the managing chat's sessions. A session or
service that does not survive is a defect to fix in a release before the
operation continues to the next host.

**5. Finish line.** The next release reaches every awake machine unattended
and the Mac catches up on wake. Initial PC adoption is strictly last. Ordinary
PC policy follows the Pi and VPS and does not wait for an offline Mac. Confirm
every machine is on the latest release and `mesh update fleet` exits 0. Add
the fleet command to the managing chat's hourly check. Then run unit 13.

**Stop conditions.** A hop that rolled back stops the operation at that host,
and the hosts that require it stay where they are. A failed rollback is
reported to managing at once. Nothing is retried by raising a timeout, editing
a journal or a record, or running an update command by hand.

## Out of scope

- A central fleet database or a permanent coordinator.
- Downgrades. A host never installs an older release.
- Pre-release or alternate channels.
- Automatic repair after a failed rollback. The helper keeps both executables
  and reports; a person decides.

## Risks

1. **Removing the coordinator policy** deletes recently merged work (#395 and
   the binary half of #414). It was never enrolled on the fleet. The reviewer
   accepted host autonomy; unit 13 is cleanup after the fleet has moved.
2. **Sigstore continuity.** Keyless signing needs GitHub and Sigstore at
   publication. Hosts verify archived bundles offline and authenticate trust
   changes through TUF. Trust-refresh failure holds activation and is reported;
   it never permits an unverified replacement key.
3. **Old seeds may not pass today's harness.** The ladder reaches back to
   about v0.1.185. Unit 3 proves each seed before committing the list, and
   fixes the harness or the candidate where one fails.
4. **A seed rollback boundary may fail.** Unit 4 runs every boundary from the
   fleet seeds first. A failure needs a compatible migration before rollout.
5. **Hosted runners and real service managers.** Unit 8 starts by confirming a
   systemd user manager and the launchd `gui` domain. The Mac's launchd domain
   must also accept the helper restart from an SSH login; the Mac pair in unit
   8 covers it.
6. **Proof time grows** from 8 sources per platform to the window plus the
   releases since the newest anchor, each with boundary runs. Unit 3 measures
   and shards.
7. **Restart frequency.** Releases publish two or three times a day. The
   30-minute check and the PC's two-hour soak collapse bursts. Unit 8 proves
   reconnection before the default is enabled.
8. **A release must follow each adoption.** The Mac and the PC wait for one.
   At the current pace that is hours.
9. **A dead canary holds its dependents.** That is deliberate and reported.
   Removing it from a policy is the owner's local change.

## Review resolution

Findings from the review of `a47e958e`.

| Finding | Resolved in |
| --- | --- |
| 1. Manual PC activation | Adopting the existing fleet; Rollout; unit 10 |
| 2. Signing required | Signed releases; release gate rule 4; unit 1 |
| 3. Discovery for arbitrarily old updaters | Stable discovery; Routes; unit 2; unit 8 scenario 4 |
| 4. Legacy-readable forward receipts | State migration and rollback; unit 4; unit 8 scenario 1 |
| 5. Rollback at every migration boundary | State migration and rollback; unit 4 |
| 6. Old live workers | Sessions and services across hops; unit 8 scenario 3 |
| 7. Fleet order | Fleet order; unit 7; unit 8 scenario 5 |
| 8. Quarantine the failed hop | Failure handling; `FailedEdge`; Routes step 3; unit 6 |
| 9. Usable publication policy and real edges | Supported releases, anchors and the release gate; unit 3 |
| 10. Policy-off fence | Host updater, the fence; unit 6 |
| 11. Installed service and connection survival in CI | Unit 8; Rollout step 4 |
| 12. Keep manual joining | Unit 13 |

The notes on package-owned and client-only installations are answered in
Installation kinds.

The final review of `ae7fcee7` accepted this design after six bounded
amendments, applied by the coordinator: complete Sigstore bundles and TUF
continuity; acyclic bounded pages and resumable epoch traversal; historical
unsigned-source exemptions; actual candidate/helper rollback boundaries;
adoption confirmation and retained operation milestones; and durable ownership
before acceptance with idempotent ledger reconciliation. No further
architecture cycle is required. The implementation PRs still require their
independent reviews and green CI.

## Evidence

### Darwin cutover closed as not reproducing

Managing closed this diagnosis on 2026-11-12. No product fix is claimed.
The original [Darwin cutover failure](https://github.com/ShaulLavo/mesh/actions/runs/43286962367/job/131423622571)
on PR 455 head `c44f5235` remains preserved in
[artifact 13976891071](https://github.com/ShaulLavo/mesh/actions/runs/43286962367/artifacts/13976891071).
The final fleet command returned 2 with `Stopped=true`, but its surviving state
did not retain the first stop reason. Two replacement daemons started less
than 0.3 seconds before fixture teardown. That timing does not identify the cause.

Both bounded follow-up controls passed:

- The current-only Linux fixture at composition `d789fcee` completed all four
  targets with the original 120-second outer bound. Its actual-daemon positive
  control captured an expected first stop before the measured cutover. The
  cutover itself never stopped. The report, corrected reproducer, per-host logs,
  observer evidence and cleanup are in
  `/work/reports/mesh-auto-update/current-cutover-first-stop/REPORT.md`.
- PR 446 head `15c81e0c` passed the normal
  [native Darwin cutover](https://github.com/ShaulLavo/mesh/actions/runs/43287823060/job/131427338690),
  including its historical workers and session-preservation checks. That run's
  separate historical update proofs failed and remain blocking.

The original cause remains unknown. Do not run another Darwin diagnostic for
this closed observation. If normal CI reproduces it, retain that artifact and
bring it back to managing. This decision changes no timeout, assertion,
authentication requirement or release gate.

### Pre-policy deployment startup did not reproduce

PR 457 head `c4b729ef` failed the replacement mode of
`TestPrePolicyDeploymentSurvivesReplacementAndManagedActivation` in
[its race job](https://github.com/ShaulLavo/mesh/actions/runs/43294894210/job/131450455936).
The daemon socket stayed absent for the existing ten-second probe, and cleanup
reported exit 1. Its temporary daemon log was not retained.
Two focused Go 1.27.4 race attempts on byte-identical deployment code passed
both replacement and managed activation, with empty daemon logs and verified
cleanup. Coordinator source review found no demonstrated cause. The observation
is not reproducing; no startup fix, port-contention cause or longer wait is
claimed. Preserve the original job and the controls in
`/work/reports/mesh-auto-update/prepolicy-deployment-race/REPORT.md`.
Normal current-source green CI remains required before merge.

### Native helper rollback health did not reproduce

PR 435 caller `7db3d671` failed
`TestOriginalInodeSurvivesRollbackRetryAndCommit` in its
[normal Darwin helper recovery job](https://github.com/ShaulLavo/mesh/actions/runs/43327607816/job/131564070619).
After the deliberately unhealthy candidate, the source health check reported
unexpected end of JSON input and a context deadline after 1.46 seconds, yielding
`rollback_failed` instead of rollback. No Go race warning was recorded.
Two focused Go 1.27.4 Linux race attempts passed the unchanged inode, rollback,
retry, commit and worker checks, including the real OpenSSL branch. The
300-millisecond health deadline stayed unchanged. Source review identifies a
possible empty-file window while the fixture writes its health JSON, but does
not establish the Darwin deadline's cause. This observation is not reproducing;
no runtime fix, deadline expansion or EOF suppression is justified. Preserve
the original failure and both passes in
`/work/reports/mesh-auto-update/helper-inode-fixture/REPORT.md`. Normal reviewed
current-source CI remains required before merge.

`/work/reports/mesh-auto-update/design-evidence/` holds the manifest fetcher,
the graph analysis, its output for v0.1.306, and the PC's read-only inventory.
The graph analysis reads manifest claims and does not verify receipts.

Checked for this revision: the receipt fields of v0.1.60, v0.1.257 and
v0.1.306; the `digest` field of the release asset API; the PC's SSH aliases
and the `t3 thread send` and `gh attestation verify` commands, by their help
output only. From primary documentation: GitHub's artifact attestation pages
and the `actions/attest` README for the action, its permissions and its use of
the Sigstore public good instance for public repositories; Sigstore's Fulcio
certificate extension list; and Sigstore's published trusted root for the
authority's validity.

No product check ran for this prose-only design, and no fleet machine was
changed. Every path, function and line cited was read on main `514700f4`.

The final signature contract follows [Sigstore's bundle requirements](https://docs.sigstore.dev/about/bundle/),
the [maintained Go verifier](https://github.com/sigstore/sigstore-go/blob/main/docs/verification.md),
and [TUF's root continuity rules](https://theupdateframework.github.io/specification/latest/).
