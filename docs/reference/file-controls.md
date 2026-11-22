# File controls

Each Mesh daemon serves five file controls over its control connection. They run as the daemon's OS account on the host that answers, so an agent reaches the files that account can reach. `host.info` reports `filesSupported: true` on a daemon that serves them. Scoped and update-only grants are refused before any handler runs.

| Control | What it does |
| --- | --- |
| `fs.read` | Returns one page of a regular file. |
| `fs.list` | Returns one page of a directory, in name order. |
| `fs.search` | Finds lines that match a Go regular expression under a directory or in one file. |
| `fs.write` | Replaces or creates a file. |
| `fs.patch` | Applies exact `oldText` to `newText` edits to a file. |

Every request names `target.hostId` and may name `target.account`. A request for another host answers `host.identity_mismatch`, and one for another account answers `file.target_mismatch`. Every result repeats the target the work ran on. Paths are absolute. The daemon resolves symlinks in a path's parent directories once, so paths such as `/tmp` on macOS or a linked workspace root work, then walks the resulting canonical path from the root through directory descriptors, opening every component without following links and doing all later work relative to the directory it opened. A component swapped for a symlink after that point is refused, and nothing reaches the directory it points at. Results and cursors report and bind the canonical path. `fs.list` and `fs.search` resolve the whole path, so a linked root such as `/work/repos` works. `fs.read`, `fs.write` and `fs.patch` never resolve the last element: a symlink there, a device, a socket or a named pipe answers `file.not_regular`, and the error names a symlink's target. The last element must be a regular file (a directory for `fs.list` and `fs.search`). Symlinks inside a searched tree are skipped. The daemon account needs read and search permission on every directory in the path.

## Reading

A page holds at most 1 MiB of raw bytes. `start` and `end` are byte offsets and `end` is exclusive. The page is `utf-8` text when its bytes are valid UTF-8 without NUL and the JSON-escaped text stays within 2 MiB; otherwise it is `base64`. A text page never ends inside a character, so its `end` can be a few bytes short of the requested length.

`nextCursor` continues from `end`. A cursor is a fixed-size token tied to the host, account, a digest of the path and the file revision, and a cursor without a revision is refused. When the file was replaced, changed in place or grown, the continuation answers `file.changed`, and the reader starts again from offset 0. The page and its revision come from one open file, and the revision is checked again before the reply.

`sha256` is the whole-file digest, included for files up to 4 MiB, or for any size when the request sets `digest`. Pass it as `expectedSha256` to `fs.write` or `fs.patch`.

## Writing and patching

`fs.write` carries at most 1 MiB of raw data, as `utf-8` or `base64`, and exactly one of:

- `expectedSha256`: the file exists and its digest matches.
- `createOnly`: the file does not exist.

The daemon writes a temporary file in the target's directory, syncs it, and installs it. `createOnly` installs with an atomic no-replace rename. A digest write swaps the staged file with the target in one atomic step and then checks that the file it displaced is the version whose digest matched; if an editor saved in between, the swap is undone, the editor's version stays, and the write answers `file.conflict`. On a filesystem that cannot swap, the daemon checks and then renames, and a save in the gap between the two calls can still be overwritten. If a third writer replaces the file before the swap can be undone, the displaced version is kept in a `.mesh-fs-` file in the same directory and the error names it. A failed precondition leaves the file unchanged, and every failure removes the temporary file through the directory it was created in. The request and the device's grant are checked again immediately before the install, and a write that waits for the daemon's commit slot stops when its caller disconnects. The result describes the file this write installed. An existing file keeps its mode bits, including setuid, setgid and sticky, and its new owner is the daemon account; a new file is `0644`.

`fs.patch` takes a file of at most 4 MiB, a matching `expectedSha256` and up to 64 edits. Each `oldText` must occur exactly once in the text as the earlier edits left it, and overlapping occurrences (`aa` in `aaa`) count as more than one, otherwise the patch answers `file.patch_mismatch` and nothing is written.

## Searching and listing

Search uses Go's `regexp` package and never starts a process. Every directory entry it looks at, file or directory, counts against a budget of 10,000, and it reads directories only as far as that budget allows. It returns at most 1,000 matches (and at most 1 MiB of them, counted as serialized JSON), reads at most 256 MiB, and skips files over 4 MiB and files with a NUL in their first 8,000 bytes. `truncated` names the bound that stopped it: `files_limit`, `matches_limit`, `bytes_limit` or `reply_limit`.

Listing returns up to 1,000 entries per page. Its cursor is the last name returned. Entries report `file`, `dir`, `symlink` or `other`, and symlinks are not followed.

## Errors

| Code | Meaning |
| --- | --- |
| `file.not_found` | The path or its parent directory does not exist. |
| `file.not_regular` | The path is a symlink, special file, or the wrong kind. |
| `file.changed` | A cursor's file was replaced or changed. |
| `file.cursor_invalid` | The cursor is malformed or belongs to another host, account, path or operation. |
| `file.conflict` | The digest or `createOnly` precondition failed. |
| `file.patch_mismatch` | An `oldText` is absent or ambiguous. |
| `file.permission_denied` | The daemon account may not do this. |
| `file.target_mismatch` | The request names another account. |
| `host.identity_mismatch` | The request names another host. |
| `resource.limit` | A size bound was exceeded. Use `mesh cp` for larger transfers. |
| `usage.invalid` | The request is malformed. |

## Contexts and the MCP tools

A request may name an execution context in `target.contextId` and carry the application caller credential that owns it. The daemon authorizes the context on this host, refuses a closed one (`context.closed`) or another caller's (`context.not_found`), and resolves a relative path against its working directory. Absolute paths still work inside a context: it names a workspace, not a sandbox. Results repeat `target.contextId` and `target.cwd`, the workspace the work ran in.

The native MCP server (`mesh mcp`) offers these controls as `mesh_file_read`, `mesh_file_list`, `mesh_file_search`, `mesh_file_write` and `mesh_file_patch`, through `HostFiles` in `internal/cli`, with a `context` handle argument. `mesh_copy` moves larger files with `copy.request`, and `mesh_fleet_status`, `mesh_profile_get` and `mesh_profile_set` read and replace this machine's host and fleet notes at an expected revision. `docs/reference/mcp-tools.md` lists every tool.
