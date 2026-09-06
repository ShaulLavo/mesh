# T16 — SFTP and SCP

**Status:** complete · **Depends on:** T11, T15 · **Owns:** `internal/sshfs/`

## Goal

`sftp -P 2222 pi.mesh.shaulavo.dev` browses that machine's served roots.
SFTP-capable file managers and `scp` use the same roots. An SSH configuration
that sets D24's port removes the need for `-P 2222`.

## What is served

Exactly what T11 declared, and nothing else (D19). This task adds a second front
door onto existing configuration; it does not add a second notion of what is
shared. A `static` or `files` service rooted at `/srv/blog` appears as `/blog`
over SFTP. A `proxy` service has no filesystem and does not appear at all.

Read-only in v0. Writes are a separate decision about a separate blast radius,
and nobody has asked for them.

## Responsibilities

1. **Subsystem wiring.** SFTP is not a Wish middleware. Register `pkg/sftp` as
   the `sftp` subsystem handler on the T15 server. Wish's `scp` middleware
   handles the `scp` side.
2. **Root confinement.** Every path resolves inside a declared root or fails.
   Reuse T11's resolver. SSH uses `LiteralRootPath` operations so filenames
   retain their literal bytes; HTTP's `OpenRootEntry` decodes URL escapes before
   using the same confined open. Canonical names returned for Realpath must
   not be reopened by name.
3. **A synthetic top level.** With more than one service the SFTP root is a
   directory listing the service names. It is generated, not a real directory,
   and it must not be escapable with `..`.
4. **Nothing else on the disk.** A client that asks for `/etc/passwd`, or for a
   symlink pointing at it, gets a permission error.

## The part that needs care

SFTP hands out a filesystem API, so every traversal bug in T11 becomes worse
here: an HTTP handler that leaks one file leaks one file, an SFTP server that
leaks a root leaks a tree.

Test the same table T11 tests, plus what only SFTP can express: absolute paths,
`..` in `Realpath` and `Stat` and not just `Open`, symlinks that resolve outside
the root, symlinks created inside the served tree by someone else, and
`Readlink` on all of them.

## Acceptance

- Go tests driving a real `pkg/sftp` client against the server.
- A traversal table covering absolute paths, `..` through every SFTP operation
  that takes a path, and symlinks out of the root. All must fail closed.
- Two services produce a two-entry synthetic root, and `..` from inside one does
  not reach the other's parent or the real filesystem.
- Every write operation is refused.
- A service whose directory has been deleted reports an error on access and does
  not take down the SSH server.
- Manual check worth doing once: mount it in a real file manager. Automated tests
  do not catch a client that dislikes your `Readdir`.

## Out of scope

Writes, per-service ACLs, and anything about HTTP. T11 still owns what is served.

## Dependencies

`github.com/pkg/sftp` provides the SFTP protocol implementation. Wish v2 provides
the SCP middleware and uses `charm.land/ssh`, so both SSH imports move together.
This upgrade keeps Bubble Tea v1's terminal queries out of process initialization
and preserves T25's silent hook startup.

Two [local dependency patches](../../third_party/README.md) fix SCP directory
ancestry and SFTP FSTAT. The copies include upstream licenses and tests.
`scripts/check-ssh-dependencies.py` verifies each copy against its pinned module
archive plus the recorded patch.

## Implementation

`internal/sshfs` exposes the live T11 registry through the authenticated T15
listener. Static and files routes appear at their declared names, including
nested names such as `/projects/site`. The top level and intermediate route
directories are synthetic. Proxy routes are omitted. Service creation,
replacement, and removal affect subsequent requests without restarting SSH.

Every file open uses `serve.LiteralRootPath.Open`, the confined implementation
shared with HTTP. Percent signs and URL-like escapes remain literal SSH filename
bytes. `Lstat` and `Readlink` resolve symlink contents before processing parent
segments, matching file opens. Canonical replies contain virtual service paths
instead of host paths. A packet guard rejects raw parent traversal before
`pkg/sftp` can clean the request path. Outside symlinks and every write operation
are refused. Deleting a served directory makes access fail while SSH stays up.

FSTAT reads metadata from the open file or directory descriptor until CLOSE,
including after a rename, unlink, or service replacement. Synthetic directory
handles retain their virtual metadata. Recursive legacy SCP preserves sibling
directories with common prefixes, such as `a`, `ab`, and `abc`.

SCP uses Wish v2's middleware with the same confined filesystem. Both modern
`scp`, which uses SFTP, and `scp -O` use the declared service paths:

```bash
sftp -P 2222 pi.mesh.shaulavo.dev
scp -P 2222 pi.mesh.shaulavo.dev:/blog/index.html .
scp -O -P 2222 -r pi.mesh.shaulavo.dev:/blog ./blog-copy
```

These commands require the authorized identity configured as described in the
[SSH front door](../plan/04-ssh.md#host-key-and-who-gets-in).

`internal/sshfs` tests drive a real SFTP client and raw protocol requests.
`integration/ssh_files.sh` exercises stock OpenSSH SFTP, modern SCP, and legacy
SCP against the daemon, including recursive downloads, write refusal, traversal,
service removal, and a deleted directory. `scripts/check-t16.sh` retains the
focused checks.

The review regressions reproduce the defects before their fixes and pass
afterward: SCP sibling placement, literal percent filenames, symlink resolution
order, FSTAT after pathname replacement, and premature SCP channel closure.
Legacy SCP now waits for client acknowledgements before advancing through the
transfer and before reporting success. A repeated stock-client check failed on
download 9 before this fix and passed all 80 downloads afterward.
Additional handle tests cover
unlink, live metadata changes, service replacement, directories, and CLOSE.

Verified on 2026-09-06: `go test -race ./...`, `go vet ./...`,
`go mod tidy -diff`, all 37 integration scripts, and `scripts/check-t16.sh`
passed. Stock SFTP, modern SCP, and legacy SCP clients completed recursive
transfers. The T25 dependency guard passed. Changed packages are lint-clean;
full lint retains eleven existing findings in bootstrap and CLI code.

A GUI file-manager mount has not been tested. The stock-client checks do not
establish compatibility with a particular file manager's directory browser.
