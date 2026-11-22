# Host MCP servers on one machine

Mesh can run an MCP server on the machine that has its tools, files and
credentials, and supervise it there. The owning daemon stores the server's
definition, starts it when a caller needs it, checks that it speaks the
stateless MCP `2026-07-28` protocol, and routes each caller's requests to the
right instance. Other devices reach it with `mesh mcp connect` (M8) or through
the aggregate gateway (M9).

## Publish a stdio server

```sh
mesh mcp publish repo-tools --cwd /work/app -- ./repo-mcp
mesh mcp ls
mesh mcp show repo-tools
```

The command, working directory and environment are resolved on this machine.
Publishing again with the same name stores a new revision. Calls already in
progress finish on the old process (at most 30 seconds); new calls start a
process at the new revision.

| Flag | Effect |
| --- | --- |
| `--cwd DIR` | Working directory. The default is the daemon account's home directory. |
| `--env NAME=VALUE` | Plain environment variable. Repeat as needed. |
| `--secret-env NAME=/abs/file` | Read `NAME` from a file at launch. Needs `--account-mode host-account`. |
| `--shared` | One instance for every authorized caller. Without it, each application caller gets its own. |
| `--kept` | Keep the server running and restart it with backoff. Needs `--shared`. |
| `--idle D` | Stop an on-demand instance after `D` without use. The default is 15 minutes. |
| `--ready-timeout D` | Time allowed for `server/discover` to answer. The default is 30 seconds. |
| `--account-mode none\|host-account` | Whose upstream account the server uses. |
| `--account-label TEXT` | Non-secret name of that account, shown to callers. |

HTTP definitions (`--url`, `--run`, `--header-secret`) take a loopback URL
only. They are refused until the host's build includes the HTTP upstream
client.

## Instances and callers

An instance belongs to one application caller: the agent frontend's own
credential on an authenticated device or the local account. Two agents on the
same device get separate processes. A caller reconnecting with the same
credential reaches its existing instance. With `--shared`, every authorized
caller uses one process, while request IDs, progress, subscriptions and
multi-round input state stay separate per caller.

An on-demand instance starts on its first call. Its idle timer starts once
active calls, acknowledged `subscriptions/listen` streams and leases all
reach zero. A lease can name a native job so the instance stays up until that
job ends.

## Readiness and failures

A started process is not ready until it answers `server/discover` with
`2026-07-28`. A server that answers with a legacy protocol is reported as
`incompatible` and is not retried until you start it again. A server that
exits early is reported with its exit status; status 127 means its command or
runtime was not found.

```sh
mesh mcp start repo-tools
mesh mcp stop repo-tools
mesh mcp restart repo-tools
mesh mcp logs repo-tools
mesh mcp rm repo-tools
```

`mesh mcp logs` prints the instance's private stderr ring (the last 1 MiB).
It needs access to the instance. Stderr never appears in the catalog or in
Mesh's own logs. Secret values and secret file paths never leave the host.

## What survives what

| Event | Server process | Request in progress | Definition |
| --- | --- | --- | --- |
| Client disconnect | Keeps running while needed | Cancelled at the server | Kept |
| Daemon restart or upgrade | Adopted by its exact reservation | Reported as `request.outcome_unknown`, never replayed | Kept |
| Server crash | Gone; started again on the next call, or with backoff when kept | Fails | Kept |
| Host reboot | Gone; kept servers start again | Lost | Kept |

If the daemon cannot tell whether a previous worker still runs, the instance
is `uncertain` and Mesh refuses to start a second one until it can.

## Secret files

A secret file must be a regular file owned by the daemon account with mode
`0600` or stricter. Mesh opens it without following symlinks and reads it only
at launch. Per-user OAuth (`--account-mode per-user`) is refused: Mesh never
substitutes a host token for a user's own account.

Hosted servers run as the daemon account without a sandbox. Anything else
running as that account can read their environment.
