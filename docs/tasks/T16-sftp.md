# T16 — SFTP and SCP

**Status:** not started · **Blocked by:** T11, T15 · **Owns:** `internal/sshfs/`

## Goal

`sftp pi.mesh.shaulavo.dev` mounts that machine's served roots in Finder,
Nautilus, or Files on Android. `scp` works against the same roots.

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
   Reuse T11's resolver rather than writing a second one. Use `OpenRootEntry`
   for operations that access an entry; `ResolveRoot` returns canonical names
   for Realpath-style results and must not be reopened by name. If these APIs
   are not reusable, that is a bug in T11 worth fixing here.
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
