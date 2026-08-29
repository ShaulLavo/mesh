# T07 — The product CLI

**Status:** not started · **Blocked by:** T04 · **Owns:** `cmd/mesh/`, `internal/cli/`

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
