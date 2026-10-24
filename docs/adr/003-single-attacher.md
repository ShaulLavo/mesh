# 003. One attacher per session, steal on attach

Status: accepted
Date: 2026-08-29

## Context

Several clients attached to one session force a resize policy and an input policy. Mesh had neither, and a silent second attach is a bug if it takes a session away from someone.

## Decision

A session has at most one attached client. A second client that attaches evicts the first, which is told `stolen`. Terminal windows (`mesh --window`) never take over silently: they attach with a detached-only request, which the worker refuses atomically if a client is attached, and the picker lists attached sessions last with an explicit take-over.

## Alternatives considered

- **Multiple attachers with mirrored terminals.** Pair debugging would work, but every resize and keystroke would need an arbitration rule. Revisit when someone needs it.

## Consequences

There are no mirrored terminals. Opening a window cannot yank a session out of another window. A keybind that restores three sessions opens three windows, each of which offers what is still detached.
