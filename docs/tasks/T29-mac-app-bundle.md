# T29 — Run the macOS daemon from Mesh.app

**Status:** implemented 2026-10-02 · **Prerequisites:** T10, T26

## Outcome

On macOS, System Settings shows Mesh with its own name and icon wherever it
asks for a permission (Full Disk Access, Local Network, and the rest). Before
this, the daemon was a bare binary, and Settings drew the generic executable
icon, because it reads a name and icon only from an app bundle.

## Design

The daemon builds the bundle itself and runs from it. `internal/macapp`:

- On `mesh daemon` start on darwin, `macapp.Enter` builds
  `<state>/Mesh.app` if its binary or recorded install path differs from the
  running binary: `Contents/MacOS/mesh` (a copy), `Info.plist`
  (`dev.shaulavo.mesh`, `LSUIElement`, so no Dock icon), `Resources/mesh.icns`
  (embedded in the binary), `Resources/installed-executable` and
  `Resources/installed-sha256`. It seals the bundle with an ad-hoc
  `codesign --force --sign -`, then `exec`s the daemon from the bundle, keeping
  the PID launchd tracks.
- Signing rewrites the copy's bytes. Staleness compares the installed binary
  with the recorded digest, and build identity (`release.Current`) hashes the
  installed binary, so the update health probe sees the digest the release
  manifest pins. The first cut hashed the copy, and an update to it rolled back
  on the health deadline.
- The rebuild stages `Mesh.app.new-<pid>` and swaps directories. Running workers
  keep their mapped binary, so a rebuild never disturbs live sessions.
- If the bundle cannot be built or signed, the daemon logs it and runs
  unbundled. A missing icon must not stop sessions.

What stays as it was (T10's release contract and T26's updater):

- Archives still hold one regular file named `mesh`. Installers, the launchd
  plist and the updater still own `~/.local/bin/mesh` as a plain file.
- After an update the service restarts, and the next daemon start rebuilds the
  bundle from the new binary.
- `macapp.Installed()` returns the installed path (from the bundle's marker) to
  every caller that means "the installed mesh": the updater, update notices and
  coordinator, update-helper bootstrap, `mesh add` uploads, agent hooks and setup,
  shell init and wake. Worker spawn and recovery keep `os.Executable()`, so
  sessions run from the bundle and macOS attributes them to Mesh.app.

## Known limits

- The ad-hoc signature changes with every binary, as the Go linker's did before,
  so macOS may ask for permissions again after an update. A Developer ID
  signature would keep grants across updates.
- The first start after this lands moves the permission holder from
  `~/.local/bin/mesh` to `Mesh.app`, so permissions are granted once more, now to
  Mesh. Instructions that name `~/.local/bin/mesh` as the binary to grant Full
  Disk Access should name Mesh instead.

## Icon

`internal/macapp/assets/mesh.svg` is the master (traced from the chosen render,
on Apple's 1024 grid). `scripts/mac-icon.sh` renders it with `rsvg-convert` and
packs `mesh.icns` with `go run ./internal/macapp/cmd/icns`; commit both.

## Verification

- `go test -race ./internal/macapp/`: build, no-op resync, rebuild after the
  binary changes, failed signing keeps the previous bundle, marker resolution,
  numeric `CFBundleShortVersionString`.
- On a macOS 26.4 arm64 host with an isolated `MESH_STATE_DIR`: the daemon
  re-executed into `Mesh.app/Contents/MacOS/mesh`; `codesign --verify --strict`
  passed (`Identifier=dev.shaulavo.mesh`, ad-hoc); `plutil -lint` passed; a
  restart reused the bundle; a session's worker ran from the bundle and survived
  a daemon restart.
- Not machine-checked: the icon in the System Settings permission panes. That
  needs a person looking at Settings after the release lands.
