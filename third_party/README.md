# Dependency patches

The main `go.mod` replaces three modules with these local copies so ordinary
`go build` and release builds include the fixes. Each directory contains the
complete Go module archive, including upstream licenses and tests.

| Module | Pinned version | Local change |
| --- | --- | --- |
| `charm.land/wish/v2` | `v2.0.3` | SCP directory ancestry checks preserve siblings such as `a` and `ab`. The sender waits for client acknowledgements before advancing or closing the channel. |
| `github.com/pkg/sftp` | `v1.13.11` | RequestServer uses an open handle's optional `Stat()` method for FSTAT, preserving metadata after pathname replacement or removal. |
| `github.com/charmbracelet/ultraviolet` | `v0.0.0-20260811164956-006e29f97886` | Terminal diffs skip long unchanged cell spans using a relative-cursor cost estimate. Overwrite candidates stop at that cursor cost; wide-character repaint safeguards remain unchanged. |

SCP waits for acknowledgement of the initial request, metadata and directory
records, file headers, and completed file contents. Client refusal stops the
transfer. A missing acknowledgement closes that SSH connection after 30 seconds,
including any sibling channels. Closing only the SCP channel cannot interrupt a
read from a peer that also withholds the SSH close acknowledgement. File data
has no transfer-duration deadline.

The patches include dependency regression tests. Mesh also exercises the fixes
through real SSH clients in `internal/sshfs` and stock OpenSSH in
`integration/ssh_files.sh`.

Mesh's terminal tests also check emitted bytes and cell counts on idle dashboard
ticks (including erase/repeat work), compare the resulting terminal cells, bound
long-span cursor allocations, and protect silent no-change frames.

Run `python3 scripts/check-dependencies.py` from the repository to download
the pinned archives, verify their recorded Go checksums, apply the patches in a
temporary directory, and compare every file against the local copies. Cached
archives work offline. The full quality gates include this check.

When upgrading, regenerate each copy and patch from the new module archive,
update `dependencies.json` and `go.mod`, and run the dependency and integration
checks. Remove the replacement and copy when upstream includes the corresponding fix.
