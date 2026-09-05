# Agent recovery compatibility

Native probes on 2026-09-05 established exact conversation resume for the installed
Codex and Claude CLIs, with different hook behavior. These are exact tested
versions and launch modes, not a minimum version or a supported version range.

| Check | Codex CLI 0.153.4, direct TUI | Codex CLI 0.153.4, pre-existing app-server | Claude Code 2.1.261 |
| --- | --- | --- | --- |
| Invocation token reaches primary hook | Yes | **No**, hook inherits server token | Yes |
| Distinct identities in one directory | Yes | Yes, two simultaneous conversations | Yes |
| Exact visible conversation restored without a new prompt | Yes, both direct test IDs | Not exercised in this mode | Yes |
| Hook confirms the ID while resumed TUI is idle | No event observed | Not exercised | Yes |
| Read-only lookup of an exact ID | `thread/read` succeeds | Same saved IDs readable independently | No native lookup adapter tested |
| Mesh automatic capture | Direct TUI only | Unavailable, use explicit binding | Enabled for the tested version |

Codex direct recovery opens the exact conversation but remains unverified until
the matching provider hook arrives. Starting its process or finding its stored ID
does not count as a resumed-session acknowledgement. The two direct test resumes
displayed their own saved replies without sending a prompt, but neither emitted a
`SessionStart` hook while idle after startup completed.

The first two Claude conversations started in 2.1.258 and resumed successfully in
2.1.261 after the installed `mise/latest` target changed during the probe. A separate
2.1.261 creation, process kill, and exact resume also passed. Only 2.1.261 has a
verified roundtrip without a version change. The probe did not run an install or
update command. The retained helper resolves the executable and records its
`--version` before every launch.

## Native evidence

The [sanitized native report](evidence/t25-native-2026-09-05.json) retains observed
hook IDs, invocation labels, lifecycle sources, and verification results. Raw
provider histories remain under the private test data roots. The probe does not
read transcripts, store prompts, or derive IDs from directory scans or screen text.
The visible-conversation checks were manual observations of the native TUIs.

Claude delivered distinct `SessionStart` IDs and invocation tokens for two
simultaneous sessions in one project. Each session produced a different short
reply. After `SIGKILL`, `claude --resume ID` displayed the corresponding saved
conversation and delivered `source: "resume"` with the original ID and new token.
A separate single-session roundtrip passed in 2.1.261 alone. The native UI retained
the selected model on resume.

Claude 2.1.261 also delivered a new ID with `source: "fork"` for
`--resume ID --fork-session`. Its `/clear` command delivered `SessionEnd` for the
old ID, then `SessionStart` with a new ID and `source: "clear"` under the same
invocation token. The `fork` value was observed directly and is part of Mesh's
accepted Claude event mapping.

Codex's stable hooks were trusted through the normal `/hooks` UI. A native
`codex app-server --listen unix://PATH` process started before either TUI client.
Both clients connected with `codex --remote unix://PATH`, using different tokens.
Both hooks inherited `daemon-before-clients`, the server's token. Their parent
PID was the server PID. Changing the client environment cannot associate these
hooks with the correct Mesh invocation.

The installed mise layout could not run `codex app-server daemon start`: that
command requires the managed standalone installation path. The private Unix
server proves the pre-existing server's environment behavior. It does not claim
to test a managed standalone daemon installation.

With no `--remote`, the same installed Codex TUI delivered the correct invocation
token, even while the private shared server remained alive. The hook appeared
when the initial prompt was submitted. After killing each direct test process,
`codex resume --cd DIR -- ID` displayed its exact saved conversation without a new
prompt. These direct resumes supplied no idle resume hook, so Mesh keeps the
confirmation state visible instead of reporting success from process startup.

An independent `codex app-server --stdio` process read both shared-server IDs using
the official JSON-lines protocol. After `initialize` and the `initialized`
notification, `thread/read` with `{"threadId": ID, "includeTurns": false}` returned
the exact requested `thread.id`, its `thread.cwd`, and an empty `turns` array.
Mesh can use this bounded read-only operation to validate an explicit Codex
binding without reading or indexing transcripts.

Separate tests exercised actual Mesh workers with each installed provider.
After worker death, the compact picker's conversation action reopened the saved
Claude ID and persisted a matching receipt with a new invocation token.
`mesh recover SESSION --agent` reopened the saved Codex ID and displayed its
history, with recovery correctly remaining unverified. Both tests retained the
source record and supplied no added prompt. The report's `mesh_end_to_end` entries
record the observed native argv and results.

## Probe commands

`scripts/probe-agent-recovery.sh [OUTPUT_PARENT]` remains offline. It runs installed
CLI help, version, and Codex schema export commands in empty data roots. Its
synthetic events and argv checks are labeled as fixtures. Its own summary always
leaves live checks unverified. Fixture success does not change provider support.

`scripts/probe-agent-recovery.sh --native --help` exposes a separate interactive
probe. `prepare` creates private provider roots and stable sanitizer hooks. It
does not launch a provider or read credentials. The explicit
`--use-existing-login` option creates symlinks to the existing credential files,
without copying their contents or linking user settings and transcripts. Providers
may refresh credentials through their normal authentication behavior.

For example, create an isolated directory and use the printed path as `PROBE`:

```sh
scripts/probe-agent-recovery.sh --native prepare /work/tmp --use-existing-login
```

Start two sessions in separate terminals. Complete the provider's normal login,
onboarding, project trust, and hook review as needed. If Claude shows onboarding
despite an existing login, completing onboarding or authenticating the isolated
root is a prerequisite. The helper does not change trust records.

```sh
scripts/probe-agent-recovery.sh --native launch PROBE claude first
scripts/probe-agent-recovery.sh --native launch PROBE claude second
```

Use a different innocuous prompt in each native TUI and wait for both replies.
Read the IDs and tokens from `PROBE/events.jsonl`. Kill those test provider
processes, then launch each exact saved ID without a prompt:

```sh
scripts/probe-agent-recovery.sh --native launch PROBE claude first-resume --resume FIRST_ID
scripts/probe-agent-recovery.sh --native launch PROBE claude second-resume --resume SECOND_ID
scripts/probe-agent-recovery.sh --native report PROBE claude first first-resume
scripts/probe-agent-recovery.sh --native report PROBE claude second second-resume
```

The report compares observed hook IDs, lifecycle sources, directories, and tokens.
It explicitly leaves visible conversation content for manual inspection. It does
not count a missing hook as a pass. `launches.jsonl` records the resolved executable,
version, native argument array, data directory, and invocation label. The helper
adds no prompt argument and does not capture terminal output.

Use `codex` in place of `claude` to test direct native launch and resume. To test
the shared-server case, first run the following command in another terminal, then
add `--shared` to both Codex launch commands:

```sh
scripts/probe-agent-recovery.sh --native daemon PROBE server-before-clients
scripts/probe-agent-recovery.sh --native read-codex PROBE EXACT_ID
```

Stop the private server after the test. Its data remains in `PROBE` for review.
No native probe command changes the user's provider hooks or production daemon.

## Limits and sources

The native tests covered completed, idle conversations. Pending work after resume,
native compaction, subagent events, worktree relocation, and managed standalone
daemon behavior remain unverified. The native probe kills provider processes.
The separate Mesh smoke tests above cover worker death and native recovery.
Account-free integration checks cover transports. No T25 VM power-loss check
was run.

The [official Codex hooks documentation](https://learn.chatgpt.com/docs/hooks)
describes the identity fields and hook trust flow. Subagent hooks can carry the
parent session ID, so an ID alone does not prove a primary event. The
[Codex command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli#codex-resume)
documents explicit resume. The generated app-server schema confirms
`ThreadReadParams.threadId` and `includeTurns`; the live lookup proves behavior.

The [Claude hooks reference](https://code.claude.com/docs/en/hooks#sessionstart)
describes session identity and startup sources. The
[session documentation](https://code.claude.com/docs/en/sessions) describes native
resume. Observed events, rather than help text alone, establish the tested token
routing and exact-ID mapping above.
