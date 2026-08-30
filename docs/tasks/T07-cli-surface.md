# T07 — The product CLI

**Status:** complete · **Blocked by:** T04 · **Owns:** `cmd/mesh/`, `internal/cli/`

## Goal

The CLI from the plan, on Cobra + Fang, replacing the hand-rolled dispatcher (D10).

```bash
m pc          # new session on host pc
m pc -r       # resume latest active session on pc
m 7K3D        # attach exact session
m             # interactive picker (T09)
m ls          # sessions across every known host
m logs 7K3D
m kill 7K3D
m wake pc
```

## The interesting part

Argument disambiguation. `m <arg>` is a host alias, a session ID, or neither.
Session IDs are 4-char Crockford base32 (`internal/session.NormalizeID`); host
aliases are user-chosen. Resolve host alias first, then session ID, then error
with both possibilities named. Reject host aliases that look like session IDs at
`mesh add` time rather than guessing later.

`m ls` per the plan: query every known online host concurrently, merge, fall back
to the local cache for offline hosts, and mark those rows clearly as stale. A slow
host must not hold up the table — deadline the fan-out and show what came back.

Host aliases live in the local config; define where and document it.

## Acceptance

- Fang wired for help, errors, completions, manpages.
- `m ls` with one host unreachable: returns promptly, shows cached rows marked
  stale, exits 0.
- Ambiguity test: a session ID that collides with a host alias produces an error
  naming both, never a silent guess.
- Existing integration scripts updated to the new surface and still passing.

## Implementation

`cmd/mesh` now gives one Cobra command tree to Fang. Fang supplies styled help
and errors, `completion`, `man`, and version output. The hidden
`session-worker` command remains in the same tree so detached workers execute
the same binary as the product commands.

The local host address book is `hosts.json` in `$XDG_CONFIG_HOME/mesh`, or
`~/.config/mesh` when `XDG_CONFIG_HOME` is unset. `MESH_CONFIG_DIR` overrides
the directory. Mesh writes the file atomically with mode `0600`. The versioned
JSON records each alias, stable host ID, Mesh identity, Tailscale name and
addresses, and the verified WebSocket endpoint.

`mesh ls` queries every address-book host concurrently under one deadline.
Successful replies replace that host's rows in the local SQLite cache. Failed
or late hosts use the cached rows and print `stale` in the table. One transport
query that ignores cancellation cannot hold the command open.

`mesh logs` resolves the session across the same host catalog. The additive
`session.logs` control returns up to 1 MiB from the worker's replay ring over a
one-shot connection. It does not acquire attach ownership, so logs cannot steal
an active terminal. For an exited or interrupted session, the daemon reads only
the requested suffix of the durable `worker.log`.

`mesh add` owns alias validation and host persistence. T08 injects a bootstrap
function that maps `bootstrap.Result` to `cli.HostRecord`; neither package
imports the other. Bare `mesh` has the same injected boundary for T09. Until
T09 lands, it returns an error that names the host and session forms you can use.
`PickerSelection` represents attach, new-session, resume, and wake actions. A
successful wake refreshes the catalog and opens the picker again.

## New dependencies

- `github.com/spf13/cobra v1.9.1` defines and parses the command tree. Keeping
  command handlers in `internal/cli` lets T09 and T14 add commands without
  rebuilding dispatch in `cmd/mesh`.
- `github.com/charmbracelet/fang v1.0.0` renders help and errors and adds shell
  completions, manpages, and build version output. This is the version fixed in
  the project stack.

`integration/cli_surface.sh` covers the offline-cache deadline, stale marking,
session-shaped alias rejection, completion generation, and manpage generation.
`integration/logs_does_not_attach.sh` proves that logs leave an attached shell
connected and responsive.
