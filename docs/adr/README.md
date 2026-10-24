# Architecture decision records

One file per decision that has a real "why". The format is in each file. Do not edit an accepted decision. Supersede it with a new record and link both.

| ADR | Decision |
|---|---|
| [001](001-workers-own-sessions.md) | Workers own sessions, the daemon only coordinates |
| [002](002-absolute-byte-offsets.md) | Sequence numbers are absolute byte offsets |
| [003](003-single-attacher.md) | One attacher per session, steal on attach |
| [004](004-ssh-front-door-on-port-2222.md) | SSH is a front door on port 2222, not the transport |
| [005](005-mesh-keys-authorize-controls.md) | Mesh device keys authorize network controls |
| [006](006-tailnet-only-hosting.md) | Hosting stays inside the tailnet |
| [007](007-innermost-detach-key.md) | `ctrl+]` detaches the innermost session |
