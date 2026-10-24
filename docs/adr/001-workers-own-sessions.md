# 001. Workers own sessions, the daemon only coordinates

Status: accepted
Date: 2026-08-29

## Context

A session must outlive the client, a crash of the daemon, and an upgrade of the daemon. If one process owned every PTY, replacing that process would kill every running command. Mesh exists so that updating or losing the coordinator costs nothing.

## Decision

Each session runs in its own detached `mesh session-worker` process that owns one PTY and the process on it. The daemon starts workers, finds them again by scanning the session directories under the state directory, and relays clients to them. Workers write their own `meta.json`, including the kernel boot ID, and the daemon reconciles it into SQLite on startup.

## Alternatives considered

- **A daemon that owns all PTYs.** It is simpler, and a daemon update would kill live work. This was the obvious design and the one Mesh rejects.
- **`systemd-run --user --scope` to detach workers.** macOS needs the same mechanism, and `Setsid` plus a directory scan is one code path for both platforms.

## Consequences

The daemon can crash, restart or update with sessions untouched. A session's fate lives in files the worker writes, so it is known while the daemon is down. State is derived: a worker whose socket answers is `running`, and a missing socket with no recorded exit is `interrupted`. A boot ID mismatch means the machine rebooted under it.

The cost is one process per session and a directory layout that has to stay compatible across versions, because a new daemon must read workers an old one started.
