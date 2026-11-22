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

Grant host-wide metadata or metadata plus session-output locally.

```sh
mesh device scope grant --identity PUBLIC_ID --permissions metadata --expected-revision 0 --json
```

- `--json`: write one agent result.
- `--identity ID`: .
- `--permissions PERMISSIONS`: .
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

## mesh device scope output

Read bounded session output over explicit-key TLS without attaching.

```sh
mesh device scope output --host HOST_ID --key-dir KEY_DIRECTORY --session SESSION_ID --format preview --json
```

- `--json`: write one agent result.
- `--host ID`: .
- `--key-dir PATH`: .
- `--session ID`: .
- `--format FORMAT`: .
- `--max-bytes N`: .
- `--cols N`: .
- `--rows N`: .
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
- `--instance INSTANCE`: inspect an exact keyed command receipt.

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

Start one keyed detached command on an online host.

```sh
mesh session create --host HOST_ID --kind pty --request-key job-17 --cwd /work --json -- /usr/bin/true
```

- `--host HOST`: online target.
- `--request-key KEY`: saved logical request key.
- `--json`: write one agent result.
- `--timeout D`: finite request deadline.
- `--kind KIND`: pty or pipe.
- `--cwd PATH`: absolute target directory.
- `--stdin MODE`: pipe stdin, closed or open.
- `--term TERM`: terminal type.
- `--cols N`: terminal width.
- `--rows N`: terminal height.
- `--shell-command TEXT`: with --identity, run through the context's shell.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.

## mesh session request

Read a durable request receipt without launching work.

```sh
mesh session request --host HOST_ID --request-key job-17 --json
```

- `--host HOST`: online target.
- `--request-key KEY`: saved logical request key.
- `--json`: write one agent result.
- `--timeout D`: finite request deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.

## mesh session wait

Wait for a durable exact-instance result.

```sh
mesh session wait --host HOST_ID 7K3D --instance INSTANCE --json
```

- `--host HOST`: online target.
- `--json`: write one agent result.
- `--instance INSTANCE`: exact creation receipt instance.
- `--timeout D`: finite deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.

## mesh session logs

Read exact bounded output bytes with independent cursors.

```sh
mesh session logs --host HOST_ID 7K3D --instance INSTANCE --json
```

- `--host HOST`: online target.
- `--json`: write one agent result.
- `--instance INSTANCE`: exact creation receipt instance.
- `--timeout D`: finite deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.
- `--stream STREAM`: pty, stdout or stderr.
- `--cursor TOKEN`: independent output cursor.
- `--max-bytes N`: page size from 1 to 1048576.

## mesh session input

Write pipe stdin once or look up a saved write key.

```sh
mesh session input --host HOST_ID 7K3D --instance INSTANCE --write-key eof --offset 0 --eof --json
```

- `--host HOST`: online target.
- `--json`: write one agent result.
- `--instance INSTANCE`: exact creation receipt instance.
- `--timeout D`: finite deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.
- `--write-key KEY`: saved logical write key.
- `--offset N`: expected byte offset, omitted for lookup.
- `--text TEXT`: exact text bytes.
- `--base64 DATA`: Base64-encoded bytes.
- `--file PATH`: read local bytes, or use - for stdin.
- `--eof`: close pipe stdin after these bytes.

## mesh session signal

Signal one exact keyed command instance.

```sh
mesh session signal --host HOST_ID 7K3D --instance INSTANCE --json
```

- `--host HOST`: online target.
- `--json`: write one agent result.
- `--instance INSTANCE`: exact creation receipt instance.
- `--timeout D`: finite deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.
- `--signal SIGNAL`: process group signal.

## mesh session kill

Stop one exact keyed command instance.

```sh
mesh session kill --host HOST_ID 7K3D --instance INSTANCE --json
```

- `--host HOST`: online target.
- `--json`: write one agent result.
- `--instance INSTANCE`: exact creation receipt instance.
- `--timeout D`: finite deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.

## mesh session rm

Remove finished output while retaining the creation receipt.

```sh
mesh session rm --host HOST_ID 7K3D --instance INSTANCE --json
```

- `--host HOST`: online target.
- `--json`: write one agent result.
- `--instance INSTANCE`: exact creation receipt instance.
- `--timeout D`: finite deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.

## mesh mcp identity create

Create a private application credential for one agent frontend.

```sh
mesh mcp identity create codex --json
```

- `--json`: write one agent result.

## mesh mcp identity ls

List local application identities without their secrets.

```sh
mesh mcp identity ls --json
```

- `--json`: write one agent result.

## mesh context open

Open a durable working context on a host for one application.

```sh
mesh context open --host HOST_ID --identity codex --request-key ctx-1 --cwd /work --json
```

- `--json`: write one agent result.
- `--identity NAME`: local application identity.
- `--host HOST`: online target; implied by a context handle.
- `--timeout D`: finite request deadline.
- `--request-key KEY`: saved logical request key.
- `--cwd PATH`: absolute directory on the target.
- `--shell SHELL`: login, sh or none.
- `--env NAME=VALUE`: overlay variable, repeatable.
- `--label TEXT`: display label.

## mesh context get

Read one context owned by the application.

```sh
mesh context get --identity codex mc1.HOST_ID.CONTEXT_ID --json
```

- `--json`: write one agent result.
- `--identity NAME`: local application identity.
- `--host HOST`: online target; implied by a context handle.
- `--timeout D`: finite request deadline.

## mesh context ls

List the application's contexts on a host.

```sh
mesh context ls --host HOST_ID --identity codex --json
```

- `--json`: write one agent result.
- `--identity NAME`: local application identity.
- `--host HOST`: online target; implied by a context handle.
- `--timeout D`: finite request deadline.

## mesh context update

Change a context's directory, environment or label at an expected revision.

```sh
mesh context update --identity codex mc1.HOST_ID.CONTEXT_ID --expected-revision 1 --cwd /work/app --json
```

- `--json`: write one agent result.
- `--identity NAME`: local application identity.
- `--host HOST`: online target; implied by a context handle.
- `--timeout D`: finite request deadline.
- `--expected-revision N`: revision before this change.
- `--cwd PATH`: new absolute directory.
- `--set-env NAME=VALUE`: set an overlay variable, repeatable.
- `--unset-env NAME`: remove an overlay variable, repeatable.
- `--label TEXT`: new display label.

## mesh context close

Stop new jobs in a context; running jobs keep going.

```sh
mesh context close --identity codex mc1.HOST_ID.CONTEXT_ID --json
```

- `--json`: write one agent result.
- `--identity NAME`: local application identity.
- `--host HOST`: online target; implied by a context handle.
- `--timeout D`: finite request deadline.
- `--expected-revision N`: fail if the context changed.

## mesh context receipt

Look up a context by the key that opened it, after a lost reply.

```sh
mesh context receipt --host HOST_ID --identity codex --request-key ctx-1 --json
```

- `--json`: write one agent result.
- `--identity NAME`: local application identity.
- `--host HOST`: online target; implied by a context handle.
- `--timeout D`: finite request deadline.
- `--request-key KEY`: request key used to open.

## mesh mcp token rotate

Rotate the bearer token for a running loopback MCP HTTP listener and retire streams using the old token.

```sh
mesh mcp token rotate --http 127.0.0.1:8765 --identity codex --json
```

- `--http ADDR`: exact loopback host:port of the running listener.
- `--identity NAME`: local application identity bound to the listener.
- `--json`: write one agent result.

## mesh session resize

Resize a keyed terminal job while no person is attached.

```sh
mesh session resize --host HOST_ID 7K3D --instance INSTANCE --cols 120 --rows 40 --json
```

- `--host HOST`: online target.
- `--json`: write one agent result.
- `--instance INSTANCE`: exact creation receipt instance.
- `--timeout D`: finite deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.
- `--cols N`: terminal width.
- `--rows N`: terminal height.

## mesh session screen

Read a keyed terminal job's rendered screen without resizing it.

```sh
mesh session screen --host HOST_ID 7K3D --instance INSTANCE --json
```

- `--host HOST`: online target.
- `--json`: write one agent result.
- `--instance INSTANCE`: exact creation receipt instance.
- `--timeout D`: finite deadline.
- `--identity NAME`: act as this local application identity.
- `--context CONTEXT`: with --identity, the job's context.
- `--cols N`: preview width up to 160.
- `--rows N`: preview height up to 24.

## mesh mcp publish

Publish or update a hosted MCP server definition on a host.

```sh
mesh mcp publish repo-tools --cwd /work/app --json -- ./repo-mcp
```

- `--host HOST`: target host (defaults to local).
- `--cwd PATH`: working directory on the target host for a stdio server.
- `--env NAME=VALUE`: plain environment variable (repeatable).
- `--secret-env NAME=PATH`: secret file path read into NAME at launch (mode 0600).
- `--url URL`: loopback Streamable HTTP endpoint of an HTTP server.
- `--header-secret HEADER=PATH`: credential header secret file sent to the HTTP server.
- `--run COMMAND`: command that starts the HTTP server at --url.
- `--run-cwd PATH`: working directory for --run.
- `--shared`: give every authorized caller one shared instance.
- `--kept`: keep the server running instead of starting on demand.
- `--idle D`: idle timeout for an on-demand instance (default 15m).
- `--ready-timeout D`: readiness probe timeout (default 30s).
- `--account-mode MODE`: none or host-account.
- `--account-label TEXT`: non-secret host account label shown to callers.
- `--json`: write one agent result.

## mesh mcp ls

List hosted MCP servers across saved hosts without waking hosts or starting services.

```sh
mesh mcp ls --json
```

- `--host HOST`: limit listing to one machine name, exact host ID, or local.
- `--identity NAME`: scope instances and private catalog cache to this application identity.
- `--timeout D`: hard deadline for fleet queries (default 2s).
- `--cached`: read private cached catalog entries without dialing hosts.
- `--json`: write one agent result.

## mesh mcp show

Show one hosted MCP server definition and its instances.

```sh
mesh mcp show local/repo-tools --json
```

- `--identity NAME`: act as this local application identity.
- `--json`: write one agent result.

## mesh mcp start

Start a hosted MCP server instance and verify stateless 2026-07-28 readiness.

```sh
mesh mcp start local/repo-tools --json
```

- `--instance HANDLE`: instance ID or mi1 handle.
- `--identity NAME`: act as this local application identity.
- `--json`: write one agent result.

## mesh mcp stop

Stop a hosted MCP server's running instances while keeping its definition.

```sh
mesh mcp stop local/repo-tools --json
```

- `--instance HANDLE`: instance ID or mi1 handle; omit to stop all managed instances.
- `--identity NAME`: act as this local application identity.
- `--json`: write one agent result.

## mesh mcp restart

Replace a hosted MCP server instance with a fresh process generation.

```sh
mesh mcp restart local/repo-tools --json
```

- `--instance HANDLE`: instance ID or mi1 handle.
- `--identity NAME`: act as this local application identity.
- `--json`: write one agent result.

## mesh mcp logs

Read a hosted MCP server instance's bounded private stderr ring.

```sh
mesh mcp logs local/repo-tools --json
```

- `--instance HANDLE`: instance ID or mi1 handle.
- `--identity NAME`: act as this local application identity.
- `--json`: write one agent result.

## mesh mcp rm

Remove a hosted MCP server definition and stop its running instances.

```sh
mesh mcp rm local/repo-tools --json
```

- `--json`: write one agent result.
