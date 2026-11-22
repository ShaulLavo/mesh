# File controls

Each Mesh daemon serves five file controls over its control connection. They run as the daemon's OS account on the host that answers, so an agent reaches the files that account can reach. `host.info` reports `filesSupported: true` on a daemon that serves them. Scoped and update-only grants are refused before any handler runs.

| Control | What it does |
| --- | --- |
| `fs.read` | Returns one page of a regular file. |
| `fs.list` | Returns one page of a directory, in name order. |
| `fs.search` | Finds lines that match a Go regular expression under a directory or in one file. |
| `fs.write` | Replaces or creates a file. |
| `fs.patch` | Applies exact `oldText` to `newText` edits to a file. |

Every request names `target.hostId` and may name `target.account`. A request for another host answers `host.identity_mismatch`, and one for another account answers `file.target_mismatch`. Every result repeats the target the work ran on. Paths are absolute. The final path element must be a regular file (or a directory for `fs.list` and `fs.search`); a symlink, device, socket or named pipe answers `file.not_regular`. Symlinks inside a searched tree are skipped, and symlinked parent directories resolve as usual.

## Reading

A page holds at most 1 MiB of raw bytes. `start` and `end` are byte offsets and `end` is exclusive. The page is `utf-8` text when its bytes are valid UTF-8 without NUL and the JSON-escaped text stays within 2 MiB; otherwise it is `base64`. A text page never ends inside a character, so its `end` can be a few bytes short of the requested length.

`nextCursor` continues from `end`. A cursor is tied to the host, account, path and file revision. When the file was replaced, changed in place or grown, the continuation answers `file.changed`, and the reader starts again from offset 0. The page and its revision come from one open file, and the revision is checked again before the reply.

`sha256` is the whole-file digest, included for files up to 4 MiB, or for any size when the request sets `digest`. Pass it as `expectedSha256` to `fs.write` or `fs.patch`.

## Writing and patching

`fs.write` carries at most 1 MiB of raw data, as `utf-8` or `base64`, and exactly one of:

- `expectedSha256`: the file exists and its digest matches.
- `createOnly`: the file does not exist.

The daemon writes a temporary file in the target's directory, syncs it, checks the precondition again, and renames it into place. A failed precondition answers `file.conflict` and leaves the file and its directory unchanged. An existing file keeps its permission bits; a new file is `0644`. The daemon serializes its own commits. Another process can still write between the final check and the rename, a window of two system calls.

`fs.patch` takes a file of at most 4 MiB, a matching `expectedSha256` and up to 64 edits. Each `oldText` must occur exactly once in the text as the earlier edits left it, otherwise the patch answers `file.patch_mismatch` and nothing is written.

## Searching and listing

Search uses Go's `regexp` package and never starts a process. It visits at most 10,000 files, returns at most 1,000 matches (and at most 1 MiB of them), reads at most 256 MiB, and skips files over 4 MiB and files with a NUL in their first 8,000 bytes. `truncated` names the bound that stopped it: `files_limit`, `matches_limit`, `bytes_limit` or `reply_limit`.

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

Code in `internal/cli` reaches these controls through `HostFiles` on a verified connection.
