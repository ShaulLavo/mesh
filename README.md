# mesh

Direct, resumable terminal sessions across your Tailscale-connected machines.

Sessions belong to the host, not to your connection. Close the laptop, lose wifi,
kill the client — the command keeps running, and you reattach to it.

## Status

Step 1 of 7 is done: **persistent local sessions**. Remote access over Tailscale
is next. See `docs/plan/02-status.md`.

```bash
go build -o mesh ./cmd/mesh

./mesh local            # start a session
                        # ctrl+] detaches; so does killing the terminal
./mesh ls               # what is running here
./mesh local -r         # reattach to the latest
./mesh kill 7K3D
./mesh sig 7K3D quit    # signals, out of band
```

## Docs

- `docs/plan/00-overview.md` — what Mesh is and the seven-step build order
- `docs/plan/01-decisions.md` — settled design decisions and what each one costs
- `docs/plan/02-status.md` — what is built, what is next, what can run in parallel
- `docs/tasks/` — self-contained briefs, one per unit of work
- `CLAUDE.md` — invariants, conventions, how to test
