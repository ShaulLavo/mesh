# Local planning evidence

`v0.1.304/mesh-release.json`, `v0.1.306/mesh-release.json` and the
`6dcd6a…` receipt are exact published release assets downloaded from the
corresponding tags at `https://github.com/ShaulLavo/mesh/releases` on
2026-11-11. The recorded source is the Linux amd64 executable from v0.1.304.

The roots, anchor page, update metadata, positions and times are **local test
wrappers**, not published signed metadata. They bind the recorded manifest
and receipt bytes without changing them. They grant no installation permission.
The maintainer command requires `-local-evidence` for this reason.

Run one planning pass (use an explicit scratch cursor outside this fixture):

```sh
go run ./cmd/mesh-release-routes -local-evidence \
  -input internal/update/testdata/discovery/source.json \
  -documents internal/update/testdata/discovery -cursor /work/tmp/routes.json
```

Remove the chosen cursor and its `.documents` directory after use. A second
pass reuses the content-addressed documents and rechecks the exact edge.
