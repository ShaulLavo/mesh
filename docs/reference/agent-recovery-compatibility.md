# Agent recovery compatibility

Provider versions that Mesh has probed for exact conversation recovery. The
how-to is [Recover an exact Codex or Claude conversation](../guides/agent-recovery.md).

## Version policy

Native probes on 2026-09-05 established exact conversation resume for Codex CLI
0.153.4 and Claude Code 2.1.261. Mesh treats these versions as a floor and
accepts later releases in the same major line. An exact pin disabled capture for
every real installation within days of a provider release.

A resumed conversation counts as verified only when the provider's own hook
reports the saved ID. A newer release that changes its hooks shows as unverified.

## Probe results

| Check | Codex 0.153.4, direct TUI | Codex 0.153.4, pre-existing app-server | Claude Code 2.1.261 |
| --- | --- | --- | --- |
| Invocation token reaches primary hook | Yes | No, the hook inherits the server's token | Yes |
| Distinct identities in one directory | Yes | Yes, two simultaneous conversations | Yes |
| Exact visible conversation restored without a new prompt | Yes, both test IDs | Not exercised | Yes |
| Hook confirms the ID while the resumed TUI is idle | No event observed | Not exercised | Yes |
| Read-only lookup of an exact ID | `thread/read` succeeds | Same saved IDs readable independently | No native lookup adapter tested |
| Mesh automatic capture | Direct TUI only | Unavailable, use explicit binding | Enabled for the tested version |

## Behavior by provider

Codex direct recovery opens the exact conversation and stays unverified until the
matching provider hook arrives. The two direct test resumes showed their saved
replies but emitted no `SessionStart` hook while idle. Starting the process or
finding the stored ID does not count as an acknowledgement.

With a pre-existing `codex app-server` (clients connect with `codex --remote`),
both clients' hooks inherited the server's invocation token and their parent PID
was the server. Mesh cannot associate those hooks with the right invocation, so
automatic capture is off in that mode. Bind the conversation explicitly with
`mesh agent bind`.

Claude delivers distinct `SessionStart` IDs and tokens for two simultaneous
sessions in one project. After `SIGKILL`, `claude --resume ID` restores the
conversation and delivers `source: "resume"` with the original ID. Claude 2.1.261
also delivers `source: "fork"` for `--resume ID --fork-session`, and `/clear` ends
the old ID and starts a new one with `source: "clear"`. Mesh's accepted event
mapping includes `fork`.

The first two Claude conversations started in 2.1.258 and resumed in 2.1.261
after the installed target changed during the probe. Only 2.1.261 has a roundtrip
with no version change. The helper records the executable and its `--version`
before every launch.

## Limits

The probes covered completed, idle conversations. These cases are unverified:
pending work after resume, native compaction, subagent events, worktree
relocation, and a managed standalone `codex app-server daemon`. No VM power-loss
check ran for agent recovery.

## Re-run the probe

Run the probe after a major provider release or a change in hook behavior.

```sh
scripts/probe-agent-recovery.sh                       # offline checks, labelled as fixtures
scripts/probe-agent-recovery.sh --native --help       # interactive native probe
scripts/probe-agent-recovery.sh --native prepare DIR --use-existing-login
scripts/probe-agent-recovery.sh --native launch PROBE claude first
scripts/probe-agent-recovery.sh --native report PROBE claude first first-resume
```

`prepare` creates private provider roots and hooks. It reads no credentials unless
you pass `--use-existing-login`, which symlinks the existing credential files. No
probe command changes your provider hooks or the production daemon. The visible
conversation check is a manual observation.

## Sources

- [Codex hooks](https://learn.chatgpt.com/docs/hooks) and [`codex resume`](https://learn.chatgpt.com/docs/developer-commands?surface=cli#codex-resume)
- [Claude hooks, SessionStart](https://code.claude.com/docs/en/hooks#sessionstart) and [sessions](https://code.claude.com/docs/en/sessions)
