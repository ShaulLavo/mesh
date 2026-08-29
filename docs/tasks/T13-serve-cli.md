# T13 — `m serve`

**Status:** not started · **Blocked by:** T11, T12, T07 · **Owns:** `internal/cli/`

## Goal

```bash
m serve pc ./site --at /blog             # tailnet only
m serve pc ./site --at /blog --public    # also at mesh.shaul.dev/blog
m serve pc 3000 --at /api --public       # proxy a local port
m serve pi /mnt/data --at /files --files
m serve ls
m unserve /blog
```

Infer the type where it is obvious: a number is a port and therefore a proxy, a
directory is static unless `--files` says otherwise. Never guess `--public`.

`m serve ls` shows name, host, kind, target, public or tailnet, and health, and
prints the actual URL for each so nobody has to assemble it by hand.

## The part that needs care

The `--public` confirmation. Publishing a directory to the internet deserves one
explicit confirmation showing the resolved path, how many files it contains, and
the URL it will appear at. Skippable with `--yes` for scripts. Nobody should ever
publish their home directory because a shell glob surprised them.

Refuse `--public` outright when the target directory contains anything that looks
like a credential (`.env`, `.git`, `id_*`, `*.pem`, `.ssh`). Refusal beats a
warning here, with a flag to override for the person who genuinely meant it.

## Acceptance

- Each form above works end to end against a real edge.
- `m serve ls` marks a service whose origin is offline, and does not hang waiting
  for it.
- The credential check catches a `.env` in the served root and refuses.
- `m unserve` removes the route from both origin and edge, and the URL 404s
  afterwards rather than hanging.
