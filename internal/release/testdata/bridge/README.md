# Bridge contract fixtures

These fixtures derive from public metadata downloaded on 2026-10-04.
The receipt claims and their manifest hashes use the current forward transition
contract. They are synthetic test evidence, not copies of published receipts.
Tests serve them from a disposable HTTPS origin and never contact GitHub.

- [v0.1.151 manifest](https://github.com/ShaulLavo/mesh/releases/download/v0.1.151/mesh-release.json).
- [v0.1.159 manifest](https://github.com/ShaulLavo/mesh/releases/download/v0.1.159/mesh-release.json).

Receipt filenames are SHA-256 hashes of their exact bytes. These fixtures test
content-addressed transition checks. They do not prove a real release upgrade or
an installed Mac's service or helper readiness. Real releases generate receipts
by opening retained state and restarting the candidate.
