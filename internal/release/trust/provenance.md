# Authenticated material history

`provenance.json` binds the unchanged embedded `trusted-root.json` to the pinned
bootstrap root. It contains successive signed roots 15 and 16, signed snapshot
166, and signed top-level targets 14 from the Sigstore public-good repository.
The target SHA-256 remains
`6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66`.
The witness stores exact bytes, so signed length and hash bindings survive JSON
serialization. These files add authorization evidence, not new default keys.

Each cached material has its own signed snapshot and targets witness. Offline
loading authenticates the saved root chain from the pinned bootstrap, verifies
the snapshot and targets signatures with the maintained TUF library, checks
delegation signatures and path scope, and checks material length and SHA-256.
Historical signed metadata authorizes archived material after its expiry. This
does not make it current. Online refresh always uses the maintained TUF client
with current expiry checks, starting from the authenticated chain and ignoring
the caller-selected Sigstore client's mutable cache. Its local probes use a
fresh pass-owned directory, removed on both success and failure.

Material order uses the earliest root revision that authenticates both the
snapshot and top-level targets, followed by the signed snapshot version. This
identifies the authorization domain and its signed revision. Claiming a later
root with unchanged keys cannot promote old metadata. A change of authorizing
keys advances the domain, including when snapshot numbering restarts. Conflicting
material at the same authenticated revision is rejected. The embedded witness
participates in this same order, so older cached definitions cannot override it.

The newest definition of each repeated CA or TSA replaces the entire previous
definition. Authority identity includes its URI and exact certificate chain.
The newest definition of each repeated CT or transparency-log ID also replaces
the previous definition. Distinct historical keys remain available. Validity
periods are not intersected; a newer signed revision may legitimately extend an
authorization as well as close it.

Signature algorithms, thresholds, target hash verification, certificate chains,
observer timestamps and SCT verification remain library operations.
