# Published bridge evidence

These files are immutable public metadata downloaded on 2026-10-04.
Tests serve them from a disposable HTTPS origin and never contact GitHub.

- [v0.1.151 manifest](https://github.com/ShaulLavo/mesh/releases/download/v0.1.151/mesh-release.json).
- [v0.1.159 manifest](https://github.com/ShaulLavo/mesh/releases/download/v0.1.159/mesh-release.json).
- [v0.1.149 to v0.1.151 Darwin receipt](https://github.com/ShaulLavo/mesh/releases/download/v0.1.151/cee03f3103d61fb1d79421e0247de58502007de90949de6890b57153cfc88a2b.json).
- [v0.1.151 to v0.1.159 Darwin receipt](https://github.com/ShaulLavo/mesh/releases/download/v0.1.159/6c17e5388adcd15b4d82039029fec8a72f908dd07d7c190a95e411b2a1169034.json).

Receipt filenames are SHA-256 hashes of the exact bytes. These receipts establish
the archive transition checks. They do not establish an installed Mac's service
or helper readiness. CLI tests inject a readiness fixture separately.
