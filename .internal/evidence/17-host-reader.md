# Original-source update reader

The hidden top-level command has two modes:

```sh
mesh update-reader -capabilities
mesh update-reader -input packet.json -trust-cache /absolute/authenticated-cache
```

Capabilities report `schema`, the compiled `epoch`, and `build` from
`release.Current()`. They do not assign a source position. The publisher owns
position assignment, packet staging and invocation of the original source
executable. The implementation schema is 1 and the current compiled epoch is 1.

Both modes bypass deployment, daemon and user-configuration initialization.
Verification is offline and read-only. The trust cache loads through the pinned
embedded bootstrap and workflow identity, with no arbitrary root option or
network refresh. A proof mode success writes a JSON result and exits 0. Invalid
arguments, incomplete evidence, unsupported epochs and verification failures
exit nonzero.

## Packet contract

The authoritative types are `update.HostReaderInput`, `HostReaderDocuments` and
`HostReaderWorker` in `internal/update/host_reader_types.go`:

```go
type HostReaderInput struct {
    Schema   int                 `json:"schema"`
    Source   HostReaderDocuments `json:"source"`
    Target   HostReaderDocuments `json:"target"`
    Forward  []byte              `json:"forward"`
    Rollback []byte              `json:"rollback"`
    Workers  []HostReaderWorker  `json:"workers"`
}

type HostReaderDocuments struct {
    Metadata []byte `json:"metadata"`
    Envelope []byte `json:"envelope"`
    Manifest []byte `json:"manifest"`
    Page     []byte `json:"page"`
}

type HostReaderWorker struct {
    Digest   string `json:"digest"`
    Protocol int    `json:"protocol"`
}
```

Byte slices are JSON base64 strings containing original bytes. Each page is
the exact asset named by that metadata's signed `page` reference. The packet
contains no executing-source identity override and no route-walk pages.

The encoded aggregate is at most 8 MiB. Each metadata document is at most
64 KiB, each signature envelope 256 KiB, each manifest 1 MiB, and each page
64 KiB. Forward and rollback proof limits use the shared
`release.MaxTransitionProofBytes`, currently 1 MiB. At most 4,096 retained
worker digest/protocol pairs are accepted. File reads enforce the aggregate
bound and reject non-regular inputs without waiting on a FIFO.

## Acceptance

`VerifyHostReaderInput` reads the source identity directly from
`release.Current()`. Signatures over the exact metadata, its manifest digest,
and the selected platform's artifact bind the signed marker to that image.
The source can use a supported older publication epoch at a waypoint, but its
marker must name the executing reader's compiled epoch and source coordinates.

The target must use the executing reader's publication epoch. Its authenticated
marker may introduce the next epoch only through the specified anchor-0
waypoint. The target must admit the source's signed position and carry the
exact forward transition for its executing platform and digest. The shared
forward and rollback verifiers check the exact bytes, full boundary coverage,
and retained worker implementations and protocols. A higher tag alone supplies
no permission.

Legacy adoption is separate. Old releases without a marker or reader command
are rejected by the autonomous reader. Their verified side supervisor must
perform full signature and rollback checks against the actual observed old
build. There is no legacy bypass flag in this command.

## Evidence

Tests build and invoke genuine Mesh executables for epochs 1 and 2. The only
test overlays replace embedded public trust material with generated fixture
material and change the compiled epoch for the epoch-2 executable. No runtime
root option or fake reader executable is involved.

The original epoch-1 executable verifies an old-format waypoint and refuses a
future-epoch target. The original epoch-2 waypoint executable then verifies a
new-epoch target using signed source position 0 while its publication remains
in epoch 1 at ordinal 7. The tests also reject an unrelated signer, missing
legacy markers and packets bound to an executable whose bytes changed.
Executing-image digests come from the child process itself.

Commands run with unusable user configuration/state paths and an unavailable
HTTP proxy. Positive verification still succeeds. The tests assert that
configuration sentinels and the staged trust directory remain unchanged.

Focused race tests cover reader inputs, actual commands, shared marker
validation and host reconciliation. Shared metadata regressions preserve
compatible unknown fields while rejecting unsupported known values and
trailing documents. Local results and cleanup receipts remain under
`/work/reports/mesh-auto-update/unit6-host-updater/`.

## Adoption record

`updates/host-adoption.json` is the sole confirmation envelope. Its pin is the
SHA-256 of exact activation update-metadata bytes, common across platforms.
A confirmed adopted milestone has origin `adoption` and `Metadata == Pin`.
Its binary remains platform-specific. A later unattended milestone has origin
`host`, a newer version, a different binary and a later commit time. Both
milestones remain bound to the envelope's operation and pin independently of
ledger pruning until the initial fleet settles.

Unit 10 owns the writer and its prerequisite image, policy and preserved-state
validation. Confirmation changes must use the existing authority lock, which
also fences host grants. Opaque JSON operation data belongs in `details`, is
bounded to 64 KiB, and is preserved by unattended reconciliation. There is no
second confirmation file.
