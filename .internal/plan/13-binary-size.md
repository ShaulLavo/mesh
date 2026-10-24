# Binary size

Status: Approved, 2026-10-08.

The owner considers the Mesh binary too big. The release binary on the x86_64
Ubuntu VPS is about 29 MB. It grew from about 27.8 MB to 29.3 MB across 28
self-updates between 2026-10-02 and 2026-10-04. This plan cuts the binary by
about 10% and the self-update download by about 24%, and adds a CI gate so
size changes are visible in review.

## Outcome

- The linux/amd64 release binary is at most 26.6 MB (from 29.47 MB).
- A self-update downloads about 8.6 MB instead of 11.3 MB.
- CI fails when the binary exceeds its budget, or when code turns off the
  linker's removal of unused methods (finding 1).
- Startup, `mesh update` and update bootstrap keep their current behavior.

## Execution state

- [x] Build and measure origin/main (`a352b66`) with the release flags.
- [ ] Step 1: size and dead-method gate in CI.
- [ ] Step 2: remove template execution from the two HTML pages.
- [ ] Step 3: publish `.tar.xz` archives and prefer them in self-update.
- [ ] Step 4: lower the budget to the measured result and record it here.

## Measurements at origin/main (`a352b66`)

Measured 2026-10-08 with Go 1.27.1, `CGO_ENABLED=0`, and the
`.goreleaser.yaml` flags (`-trimpath -ldflags '-s -w -buildid= -X …Version=…'`).

| Build | Bytes |
| --- | --- |
| linux/amd64 release flags | 29,466,748 |
| linux/amd64 without `-s -w` | 41,800,461 |
| linux/arm64 release flags | 27,328,636 |
| `mesh_linux_amd64.tar.gz` asset, v0.1.211 | 11,257,002 |

The release is already stripped, so debug symbols are not the cause. Mesh is a
single static Go binary. Its `//go:embed` payload (SQL migrations, the app pill
JS and CSS, install scripts, unit files) totals 54 KB.

ELF sections of the release binary:

| Section | MB | What it is |
| --- | --- | --- |
| `.text` | 14.10 | machine code |
| `.gopclntab` | 10.70 | Go's function and line tables, needed for stack traces and the GC; scales with code |
| `.go.type` | 2.26 | runtime type descriptors |
| `.rodata` | 1.11 | constants, strings |
| `.noptrdata` + `.data` | 1.27 | initialized globals |

Estimated on-disk share by module. Code and data come from `go tool nm -size`
on an unstripped build. Pclntab and per-function metadata are spread in
proportion to code.

| Module | MB |
| --- | --- |
| Go standard library | 11.9 |
| Mesh's own packages | 8.5 (cli 2.2, daemon 1.0, tui 0.85, apps 0.56, dnsname 0.35, the rest under 0.35 each) |
| `modernc.org/sqlite` | 4.2 (4.28 MB marginal, measured with a probe program) |
| `golang.org/x/crypto` | 0.75 |
| Charm (ultraviolet, lipgloss, x, bubbles, huh, bubbletea, fang) | about 1.5 combined |
| Every other dependency | under 0.15 each |

Two large symbols are zero-filled BSS, not file bytes:
`crypto/internal/fips140/drbg.memory` (32 MiB) and
`github.com/mattn/go-runewidth.strictWidthLUT` (2.2 MB, filled lazily). They
reserve address space only and do not affect the file size.

Growth: the amd64 tarball went from 10.30 MB (v0.1.56, 2026-09-30) to
11.26 MB (v0.1.211), about 9%. The increase came in small steps from feature
work: host metrics in v0.1.112, machine naming in v0.1.174, fleet approval in
v0.1.190. No single new dependency explains it. Finding 1 makes every feature
cost more than its own code, because each reachable type keeps all of its
exported methods.

## Findings

### 1. Template execution keeps every exported method (−2.96 MB)

Go's linker normally drops methods nothing calls. A call to
`reflect.Value.Method` or `MethodByName` with a non-constant name turns that
off for the whole program. `text/template` makes that call when it evaluates
fields, and `html/template` is built on it.
[`whydeadcode`](https://github.com/aarzilli/whydeadcode) on
`go build -ldflags=-dumpdep` shows two call sites:

- `internal/serve/handler.go:19,212`: the static-site directory listing, a
  six-line template, since v0.1.0.
- `internal/apps/http.go:418,507`: the temporary apps management page (pairing,
  frame pill, confirm forms, app list), since v0.1.54.

With both `Execute` calls removed in a scratch copy, the release binary is
26,509,436 bytes (−2,957,312, −10.0%), and `whydeadcode` reports no other
caller. `mesh version` starts in 3 ms before and after.

### 2. gzip is the weakest archive format available (−2.68 MB per download)

The same tar of the release binary:

| Compression | Bytes |
| --- | --- |
| gzip -9 (current) | 11,101,760 |
| zstd -19 --long=27 | 9,133,186 |
| xz -9e | 8,417,164 |

Self-update unpacks in Go, so a new format needs a decoder in the binary. In
probe programs, `github.com/ulikunitz/xz` adds 0.50 MB and
`github.com/klauspost/compress/zstd` adds 0.67 MB (that module is in the graph
but not linked today). xz wins on both counts. Decompressing takes about 1 s
here, which doesn't matter next to the download.

### 3. Measured and rejected

| Option | Saving | Why not |
| --- | --- | --- |
| Replace SQLite | 4.28 MB | SQLite holds host state (D7 in `01-decisions.md`, 12 migrations, goose). The pure-Go alternative `github.com/ncruces/go-sqlite3` is larger (5.23 MB marginal). |
| `-tags nethttpomithttp2` | 0.57 MB | The daemon serves HTTPS itself (`internal/daemon/runtime.go:271,284`). Browsers would drop to HTTP/1.1. |
| `-gcflags=all=-l` | 2.04 MB | Disables inlining everywhere, which slows relay and terminal paths. |
| `GOAMD64=v3` | 12 KB | No size effect. |
| UPX | not measured | Every session worker re-executes the same file. UPX unpacks into private memory per process, so workers stop sharing pages. It also breaks `debug/buildinfo` reads in `internal/updatebootstrap` and draws antivirus and notarization trouble. |
| Drop `golang.org/x/net/http2` from `internal/release/download_retry.go` | about 0.05 MB | Too small to justify touching retry classification. |
| Split into several binaries | unknown | Breaks the single-file update and re-exec model. |

## Steps

Each step is its own reviewed PR with a patch version bump.

### Step 1. Size and dead-method gate in CI

Add `scripts/check-binary-size.sh` and a CI step in the Go job:

- Build `./cmd/mesh` for linux/amd64 with the flags from `.goreleaser.yaml`.
  The script has a test that fails when its flags and the goreleaser flags
  differ.
- Fail when the size exceeds `scripts/binary-size-budget`, a byte count.
  Start it at 29,700,000. Print the size and the budget to the job summary.
- Run `go build -ldflags=-dumpdep ./cmd/mesh 2>&1 | go run
  github.com/aarzilli/whydeadcode@<pinned pseudo-version>`. Fail on any
  output. `go run pkg@version` keeps the tool out of `go.mod`.

Until step 2 lands, the dead-method check runs in report-only mode. Step 2
turns it into a hard failure.

Verify: the script fails when the budget is set below the current size, and
on a scratch change that adds a `template.Execute` call.

### Step 2. Render the two pages without templates (−2.96 MB)

Replace both `Execute` calls with code that writes HTML directly and escapes
each value for where it lands:

- Text and attribute values: `html.EscapeString`.
- URL query values (`/confirm?id=…&action=…`, `/view?id=…`):
  `url.QueryEscape`, then HTML-escaped.
- `href` values: keep the current builders (`mountedURL`, app URLs), then
  HTML-escape. Replace any value whose scheme is not `http`, `https` or
  `mailto` with `#ZgotmplZ`, as `html/template` does today.
- Values inside `<script>` (`visibility`, `owns`, `deadline`, the
  `postMessage` target): `json.Marshal`, which escapes `<`, `>` and `&`.

Keep the existing CSP, `nosniff` and `Referrer-Policy` headers unchanged.

Tests, written before the switch:

1. Golden output from the current templates for every page state: pair code,
   pair start, frame for owner and non-owner, confirm for public, private,
   renew and delete, the app list, the error banner, and a directory listing
   with a `../` entry. The new renderer must match byte for byte on these
   benign inputs.
2. Hostile inputs (`<script>`, `"`, `'`, `</script>`, `&`, `javascript:` URLs,
   and file names containing all of them) must produce no unescaped markup
   and no executable URL.

Risk: an escaping mistake on the owner-control page is a cross-site scripting
hole on the page that deletes or publishes apps. Mitigation: the tests above,
a reviewer focused on escaping, the CSP, and the existing CI confirmation-guard
browser checks (`web/app-pill/tests`).

When this lands, the step 1 check becomes a hard failure. Set the budget to
the measured size plus 1%.

### Step 3. `.tar.xz` downloads (−2.68 MB per update, +0.50 MB binary)

- `.goreleaser.yaml`: publish `mesh_<os>_<arch>.tar.xz` next to the existing
  `.tar.gz`. If one archive entry cannot emit both formats, add a second entry
  with its own id. Keep `.tar.gz` for installer hosts without an xz decoder and
  for Homebrew.
- Checksums, the release manifest (`scripts/generate-release-manifest.sh`,
  `scripts/assemble-release-compatibility.py`) and release reservation cover
  both archives.
- `internal/release`: download `.tar.xz`. Preserve checksum verification and
  download retry handling.
  Add `github.com/ulikunitz/xz` and put the size trade (finding 2) in the
  task brief, as `CLAUDE.md` requires for new dependencies.
- `internal/updatebootstrap` and `scripts/install.sh`: use `.tar.xz` when `xz`
  is on the host and `.tar.gz` otherwise. Homebrew keeps `.tar.gz`.

Require both archives in the release manifest and packaging checks. Test
installer archive selection on hosts with and without an xz decoder.

### Step 4. Record the result

Update this plan's measurement tables with the post-change sizes. Set the
budget to the final measured size plus 1%. From then on, a PR that raises the
budget states the cause in its description.

## Verification for every step

```bash
go mod tidy -diff
go test -race ./...
go vet ./...
./scripts/verify.sh
./scripts/check-binary-size.sh
```

Self-update and startup:

- `scripts/check-updates.sh` and `scripts/prove-release-transition.sh` pass
  for releases built with the current archive contract.
- An update-bootstrap run on a host with `xz` and on one without it.
- `mesh version` startup stays within 1 ms of the 3 ms baseline over 50 runs.
- A daemon restart keeps running sessions attached (`verify.sh` covers this).
- After step 2, check in a browser: a served directory listing, plus the
  temporary apps pairing, frame pill and each confirm form.

Mock tests don't prove a real self-update. Before closing this plan, the VPS
self-updates to the step 3 release and the `.tar.xz` download shows up in
the update log.
