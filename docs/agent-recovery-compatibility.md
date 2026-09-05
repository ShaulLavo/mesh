# Agent recovery compatibility

The offline part of [T25 milestone 1](tasks/T25-agent-recovery.md) has a rerunnable
probe. Automatic conversation recovery has no verified provider setup yet. The
probe does not complete the milestone's live association and resume checks.

`scripts/probe-agent-recovery.sh [OUTPUT_PARENT]` requires Bash and Python 3.
Each run creates a private `mesh-agent-recovery-*` directory under the supplied
parent, or the temporary directory by default. The script captures installed CLI
versions, help, Codex protocol schemas, documented hook field summaries, synthetic
events, constructed resume arguments, and `summary.json`.

Child processes use empty provider data directories and a minimal environment.
The probe runs help, version, and schema-generation commands. It does not start
provider sessions, connect to a daemon, install hooks, access existing transcripts,
copy credentials, or request model output. It retains its output for review.

## Observed on 2026-09-05

| Check | Codex CLI 0.153.4 | Claude Code 2.1.258 |
| --- | --- | --- |
| Exact-ID command in installed help | `codex resume SESSION_ID` | `claude --resume SESSION_ID` |
| Recorded directory | `--cd DIR` is advertised | Set the child process directory |
| Native schema export | App-server JSON Schema export succeeds | No hook-schema export appears in inspected help |
| Hook identity | Documented `SessionStart.session_id` and `cwd` | Documented `SessionStart.session_id` and `cwd` |
| Two concurrent conversations in one directory | Unverified | Unverified |
| Exact resume with no added prompt | Unverified | Unverified |
| Invocation-to-conversation association | Unverified through an existing shared daemon | Unverified |

These versions identify the installations inspected. They do not establish a
minimum version or a supported range. Help confirms command syntax exists, but
does not prove that a saved hook ID reopens the intended conversation.

The generated Codex schema includes `ThreadResumeParams.threadId` and
`ThreadReadParams.threadId`. `HookStartedNotification` and
`HookCompletedNotification` contain `threadId`, `turnId`, and `run`. These are
app-server notifications, not the JSON input schema for command hooks.
The exported experimental `ThreadStartParams` and `ThreadResumeParams` have no
top-level invocation token or hook-environment property. Their general `config`
property does not establish a safe token route. The native TUI's routing behavior
still needs a live check.

The [official OpenAI hooks documentation](https://learn.chatgpt.com/docs/hooks)
describes command-hook identity, startup sources, and trust review. Subagent hooks
can carry the parent session ID, so an ID alone does not prove a primary event.
The [Codex command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli#codex-resume)
describes explicit resume. The probe uses synthetic events based on those fields.
It does not claim to observe actual hook delivery.

The [Claude hooks reference](https://code.claude.com/docs/en/hooks#sessionstart)
describes session identity and startup sources. The
[session documentation](https://code.claude.com/docs/en/sessions) describes native
resume. Its hook field summary is documentation-derived, not an exported native
schema. Environment inheritance and lifecycle delivery remain untested.

## Evidence limits

The four saved event fixtures represent two IDs per provider in the same
directory. Fixture checks remove prompt, transcript, and environment fields.
A separate subprocess checks argument boundaries for spaces and shell characters.
This establishes that the probe can retain distinct synthetic identities and
construct argument arrays. It does not test Mesh registration, provider parsing
of opaque IDs, hook trust, or concurrent native conversations.

The constructed Codex command is `codex resume --cd DIR -- SESSION_ID`.
The constructed Claude command is `claude --resume SESSION_ID`, with `DIR` as the
child process directory. Neither command includes a prompt, fresh-session flag,
or latest-session selector. These commands are recorded, not executed.

## Remaining compatibility gate

Before automatic capture becomes available, an isolated authenticated test must
record two native conversations in one directory and reopen each exact ID without
a new prompt. Codex must additionally route two distinct invocation tokens through
a daemon that started before either terminal client. Changing an environment
variable on a client does not prove that the daemon's hooks receive it.

That check must also observe hook trust, forks and thread switches, the ID reported
after resume, and whether the provider resumes pending work. It must use stable
hook definitions and the normal trust flow. The offline probe leaves every one of
these claims `unverified`.

T25's proposed explicit `mesh agent bind PROVIDER CONVERSATION_ID` is the fallback
when automatic association cannot be proved. That command is not implemented by
this probe. Until T25 implements validated binding, native exact-ID resume remains
a manual provider action. T24 shell recovery does not advertise conversation
recovery based on these fixtures.
