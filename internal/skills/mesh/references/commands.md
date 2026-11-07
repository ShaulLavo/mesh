# Shipped commands

Generated from the implemented operation registry. Regenerate with `go generate ./internal/skills`.

## mesh device scope keygen

Create a separate metadata credential without enrolling it.

```sh
mesh device scope keygen --key-dir KEY_DIRECTORY --json
```

- `--json`: write one agent result.
- `--key-dir PATH`: explicit dedicated credential directory.

## mesh device scope grant

Grant metadata access on the target's local policy.

```sh
mesh device scope grant --identity PUBLIC_ID --permissions metadata --expected-revision 0 --json
```

- `--json`: write one agent result.
- `--identity ID`: .
- `--permissions metadata`: .
- `--expected-revision N`: .
- `--expires-at TIME`: .
- `--expires-in D`: .

## mesh device scope revoke

Remove a scoped key from local target policy.

```sh
mesh device scope revoke --identity PUBLIC_ID --expected-revision 1 --json
```

- `--json`: write one agent result.
- `--identity ID`: .
- `--expected-revision N`: .

## mesh device scope list

Read the local target's scope policy.

```sh
mesh device scope list --json
```

- `--json`: write one agent result.

## mesh device scope read

Read a narrow metadata page over explicit-key TLS without fallbacks.

```sh
mesh device scope read --host HOST_ID --key-dir KEY_DIRECTORY --json
```

- `--json`: write one agent result.
- `--host ID`: .
- `--key-dir PATH`: .
- `--section SECTION`: .
- `--session ID`: .
- `--cursor TOKEN`: .
- `--limit N`: .
- `--timeout D`: .

## mesh serve ls

List live and cached services without opening their URLs.

```sh
mesh serve ls --json
```

- `--json`: write one agent result.
- `--timeout D`: hard deadline for live host queries.

## mesh hosts

Read machine context without waking machines.

```sh
mesh hosts --json
```

- `--json`: write one agent result.
- `--timeout D`: hard deadline for host queries.
- `--cached`: do not dial hosts.

## mesh inspect

Read full machine notes and current access.

```sh
mesh inspect HOST_ID --json
```

- `--json`: write one agent result.
- `--timeout D`: hard deadline for host queries.
- `--cached`: do not dial hosts.

## mesh session list

Read a bounded page of sessions without attaching.

```sh
mesh session list --host HOST_ID --json
```

- `--host HOST`: machine name or exact host ID.
- `--json`: write one agent result.
- `--timeout D`: hard deadline from 100ms to 60s.
- `--all`: include ended sessions.
- `--detail`: include launch command and directory.
- `--limit N`: page size from 1 to 500.
- `--page TOKEN`: nextPage token from this host.

## mesh session inspect

Read live or saved session details without attaching.

```sh
mesh session inspect --host HOST_ID SESSION_ID --json
```

- `--host HOST`: machine name or exact host ID.
- `--json`: write one agent result.
- `--timeout D`: hard deadline from 100ms to 60s.
- `--preview`: include a preview at most 80 by 24.

## mesh profile show

Read this machine’s owner-written notes.

```sh
mesh profile show --json
```

- `--fleet`: read the local signed fleet replica.
- `--json`: write one agent result.

## mesh profile set

Replace this machine’s notes at an expected revision.

```sh
mesh profile set --file PATH --expected-revision 0 --json
```

- `--fleet`: publish signed notes to saved fleet members.
- `--json`: write one agent result.
- `--file PATH`: editable JSON file, or - for stdin.
- `--expected-revision N`: revision before this write.

## mesh profile edit

Edit this machine’s notes with VISUAL or EDITOR, for humans only.

```sh
mesh profile edit
```

- `--fleet`: edit signed fleet notes.

## mesh profile status

Read local fleet delivery state without contacting members.

```sh
mesh profile status --fleet --json
```

- `--fleet`: read fleet delivery state.
- `--json`: write one agent result.
- `--retry`: reset pending and stale delivery age.

## mesh skill show

Print exact embedded skill bytes without reading configuration.

```sh
mesh skill show
```

- `--file PATH`: embedded file to print.

## mesh skill install

Install the managed Mesh skill.

```sh
mesh skill install --dir ROOT --json
```

- `--dir ROOT`: existing skills root.
- `--json`: write one agent result.

## mesh skill update

Update the managed Mesh skill.

```sh
mesh skill update --dir ROOT --json
```

- `--dir ROOT`: existing skills root.
- `--json`: write one agent result.

## mesh skill status

Inspect the managed Mesh skill and its registration.

```sh
mesh skill status --dir ROOT --json
```

- `--dir ROOT`: existing skills root.
- `--json`: write one agent result.

## mesh skill uninstall

Uninstall the managed Mesh skill.

```sh
mesh skill uninstall --dir ROOT --json
```

- `--dir ROOT`: existing skills root.
- `--json`: write one agent result.

## mesh session create

Start one keyed detached PTY command on an online host.

```sh
mesh session create --host HOST_ID --kind pty --request-key job-17 --cwd /work --json -- /usr/bin/true
```

- `--host HOST`: online target.
- `--request-key KEY`: saved logical request key.
- `--json`: write one agent result.
- `--timeout D`: finite request deadline.
- `--kind pty`: explicit PTY kind.
- `--cwd PATH`: absolute target directory.
- `--term TERM`: terminal type.
- `--cols N`: terminal width.
- `--rows N`: terminal height.

## mesh session request

Read a durable request receipt without launching work.

```sh
mesh session request --host HOST_ID --request-key job-17 --json
```

- `--host HOST`: online target.
- `--request-key KEY`: saved logical request key.
- `--json`: write one agent result.
- `--timeout D`: finite request deadline.
