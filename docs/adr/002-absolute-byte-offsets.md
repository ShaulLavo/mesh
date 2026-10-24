# 002. Sequence numbers are absolute byte offsets

Status: accepted
Date: 2026-08-29

## Context

A reconnecting client has to say where it stopped, and the worker has to answer with exactly what it missed. The simplest resume state wins, because the client can lose its connection at any moment.

## Decision

`seq` counts the PTY bytes a session has emitted since it started. The worker keeps recent output in a 4 MiB ring indexed by those offsets. Resume is a slice of the ring from the client's offset. "I have everything before byte N" is the entire client-side resume state. If N has left the ring, the worker sends its rendered screen and continues live from the current head.

## Alternatives considered

- **Per-chunk counters.** They freeze the original output batching into the protocol forever, because chunk boundaries would become part of the resume contract.

## Consequences

Output can be rebatched freely, so a single offset is enough for any transport. The same frame format serves the Unix socket, WebSocket and SSH paths.

A client that stays away longer than the ring holds gets a fresh screen instead of the missed bytes. Scrollback beyond the ring is not kept by the worker.
