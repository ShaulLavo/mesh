# 007. ctrl+] detaches the innermost session

Status: accepted
Date: 2026-09-05

## Context

With `mesh --window`, every terminal window is a session, so Mesh clients nest: you attach to a session on `pc` from inside a session on your laptop. The outermost client reads every keystroke first, so deciding which client owns a key is a design choice.

## Decision

`ctrl+]` detaches the innermost session. `ctrl+^` is consumed by the outermost client and ends the window with everything inside still attached. An inner client registers with its containing worker over one extra connection (`session.nest`) for as long as it is attached, and the worker tells the outer client when that set changes. A closed connection means not nested.

## Alternatives considered

- **A fixed depth table.** It made `ctrl+^` the everyday "leave pc" key and `ctrl+]` just closed the window. Fingers expect the reverse. The table stays as a fallback for workers that predate registration.

## Consequences

The cost is one extra Unix connection per nested client, and a window about one attach round trip long in which `ctrl+]` can still reach the outer client. Both keys are flags: `--detach-key` and `--leave-key`. Vim's tag jump on `ctrl+]` needs the detach key rebound, or `--detach-key none`.
