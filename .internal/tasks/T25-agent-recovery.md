# T25 — Recover exact Codex and Claude conversations

**Status:** implemented, with native compatibility limits · **Prerequisites:** T24's checkpoint and
recovery transaction milestones · **Providers:** Codex CLI and Claude Code

Delivered on 2026-09-05. See [setup and recovery](../agent-recovery.md),
[native compatibility evidence](../agent-recovery-compatibility.md), and the
delivery notes below. The original design and acceptance brief follows those notes.

## Delivery notes

`internal/agentresume` owns bounded identity fields, provider event decoding,
supported launch options, exact resume argv, and read-only Codex ID lookup.
`mesh agent`, opt-in Bash and Zsh wrappers, stable provider hooks, and idempotent
setup commands preserve the native terminal interface. The worker validates each
foreground invocation and acknowledges registration only after its record is durable.

CLI and picker conversation actions, WebSocket recovery, and stock OpenSSH all
use the same per-invocation recovery claim. A matching native start event stores
an immutable receipt. An unconfirmed replacement stays usable and unverified,
and retries return that replacement. Closed agents can resume in a new worker
while the original shell remains available. Native resume failure offers a shell
in the saved directory and retains the original record.

Authenticated native tests restored two distinct Codex and Claude histories in
the same project without an added prompt. Separate Mesh worker crash tests
restored a Claude conversation through the compact picker and a Codex conversation
through `recover --agent`. Claude Code 2.1.261 supplied the matching resume hook.
Codex CLI 0.153.4 direct TUI displayed the saved history but supplied no idle
resume hook, so Mesh correctly retained an unverified status.

A pre-existing Codex app-server delivered its own token to hooks instead of the
client's token. Automatic capture is therefore disabled for `--remote` clients.
`mesh agent bind codex ID` validates an exact native ID and directory with bounded
`thread/read` lookup. That lookup cannot confirm a running replacement.

The native hooks exposed a production startup delay in legacy Bubble Tea package
initialization, imported through Wish. The SSH listener now uses the same
underlying Charm SSH server with logging, panic recovery, and bounded per-IP rate
limiting. Production startup no longer waits for terminal color queries before
the silent hook helper runs. A dependency check prevents that import path returning.

`scripts/check-t25.sh` retains the account-free contract checks, including real
worker death after durable registration, fake native processes, delayed hooks,
concurrent Unix/WebSocket/SSH recovery, missing conversations, and cross-builds.
The [native probe](../agent-recovery-compatibility.md#probe-commands) records actual
provider behavior separately from fixture results.

Final checks passed: `go test -race ./...`, `go vet ./...`, `go mod tidy -diff`,
formatting, all 35 integration scripts, and `scripts/check-t25.sh` including
CGO-disabled Linux amd64/arm64 and Darwin arm64 builds. Unlimited lint reports
the same eleven pre-existing findings. History, window, reboot, and SSH fixtures
now await published exit or detach metadata before dependent actions. The SSH
fixture also waits for the picker to release the terminal before accepting a
live shell prompt, so saved preview text cannot satisfy that check.

A subsequent [review](../../.audit/t25/review.md) added regressions and fixes for
repeated crashes before confirmation, new capture from the fallback shell,
unrecorded backend routing, symlinked settings, hook diagnostics, and picker
target labels. Pending plans remain separate from acknowledged identities.

No VM power-loss check was run for T25. Native pending work, compaction, subagent
events, relocated worktrees, other provider versions, and the managed standalone
Codex daemon remain outside the verified native matrix. Deterministic tests cover
identity changes and subagent rejection, but do not prove those native lifecycles.
Mesh prevents duplicate recovery within its own source claims. It does not detect
the same conversation running independently outside Mesh.

## Outcome

Run Codex or Claude inside a Mesh terminal. After the host crashes, select the
same task in Mesh and reopen that exact saved conversation in its project or
worktree. Continue using the provider's normal interactive terminal interface.

The Mesh terminal session ID and provider conversation ID are different identities.
Reopening `codex` or `claude` without the provider ID is not conversation recovery.

This plan covers local provider histories on the host where the tool ran. It does
not transfer conversations to another computer or add a cloud-agent control plane.
The sections below retain the implementation's design and acceptance requirements.

## Depend on T24 without expanding its shell model

Use [T24](T24-crash-recovery.md) for checkpoints, previous output, saved shell
directories, exact host identity, retained attempts, and concurrent recovery claims.
Both providers add a typed recovery recipe to that record. The existing worker
continues to own the PTY. The provider continues to own transcripts and authentication.

Start provider compatibility research while T24 is built. Integration implementation
depends on T24 milestones 1 and 3. Ordinary `codex` and `claude` commands inside a
shell also need T24 milestone 2's opt-in shell setup.

## Verified starting points

Checked on 2026-09-05 with installed Codex CLI `0.153.4` and Claude Code `2.1.258`.
These are observed versions, not minimum supported versions. A compatibility
probe must establish the supported range before release.

- Codex accepts `codex resume SESSION_ID`. Its documented command hooks expose
  `SessionStart`, `session_id`, `cwd`, and an optional transcript path. Hooks can
  be configured in `hooks.json` or inline TOML and require trust review. Transcript
  format is not a stable hook interface. [Codex hooks](https://learn.chatgpt.com/docs/hooks),
  [resume reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli#codex-resume).
- Claude accepts `claude --resume SESSION_ID`. Its `SessionStart` input identifies
  the conversation and directory, including starts, resumes, clears, and forks.
  [Claude hooks](https://code.claude.com/docs/en/hooks#sessionstart),
  [saved sessions](https://code.claude.com/docs/en/sessions).
- The installed Codex exposes a shared app-server daemon. A hook's process ancestry
  or inherited environment may therefore differ from the terminal that opened the
  conversation. This association is an explicit feasibility check, not an assumption.
- Mesh already exports host/session identity and has a bounded containing-worker
  lookup. The [lookup's contract](../../internal/worker/containing_session.go) warns
  against using inherited session variables as the authority.

Keep this version probe rerunnable. Do not require a model response to test parsing,
registration, or command construction. Test real provider startup and exact resume
separately when authenticated test sessions are available.

## Choose hooks and a thin launch helper

Add a small `internal/agentresume/` package with Codex and Claude event decoding,
resume-argv construction, compatibility checks, and registration helpers. Keep
provider names as a closed set. Do not build a generic plugin runtime.

The preferred flow preserves the normal commands:

1. The opt-in shell integration delegates `codex` and `claude` to a proposed
   `mesh agent PROVIDER -- ARG...` helper. Resolve the actual executable without
   recursively invoking the shell function. Plain absolute executable paths still work.
2. The helper asks the immediate worker for an invocation token and captures the
   provider executable, storage context, project directory, and selected launch options.
3. A stable provider hook command invokes `mesh agent-hook PROVIDER` with bounded
   JSON on stdin. It forwards only recovery identity fields to the worker.
4. The worker associates the observed provider ID with that exact invocation and
   saves it before acknowledging registration. It becomes recoverable only after
   that acknowledgement. The provider keeps its own transcript on disk.
5. On recovery, start the provider with its explicit saved ID. Mark conversation
   recovery successful only after the resumed provider reports the expected ID.

The installed provider must prove a per-invocation route for the token. Prefer a
stable hook definition plus verified invocation-local context. Do not bake a random
token into a new Codex hook definition on every launch, which would invalidate
hook trust. Do not assume that setting an environment variable on the TUI client
changes a pre-existing shared daemon's hook environment.

If that route is unavailable, support `mesh agent bind PROVIDER CONVERSATION_ID`
as the deterministic fallback for the containing session. Validate the binding on
that host and label it explicit. Automatic mode remains unavailable for that
provider/version until the probe proves exact association. Do not silently fall
back to directory scans, transcript modification times, screen scraping, or `--last`.

Do not replace either terminal UI with an app-server client in this task. A small
official API adapter for reading a specific ID is acceptable only if the probe
shows it is needed and can remain read-only. A new agent UI is a separate scope decision.

## Provider recipe and ownership

Extend T24's recovery record with an optional versioned `AgentRecovery` value:

| Field | Meaning |
|---|---|
| Provider | `codex` or `claude`. |
| Conversation ID | Opaque provider-issued identity verified by startup/resume. |
| Invocation token | Correlates accepted events with one launch in one Mesh worker. |
| Project/worktree directory | The provider's conversation directory, separate from the shell's directory. |
| Provider executable and observed version | Reopen the intended installation and diagnose incompatibility. |
| Storage context | The provider's configured local data root and non-secret profile selection. |
| Supported launch options | A provider-specific allowlist needed to preserve the user's session behavior. |
| Registration time and lifecycle | Distinguish active, normally closed, and explicit saved bindings. |

Use structured fields and argv. Do not save prompts, credentials, complete
environments, transcript contents, or arbitrary settings blobs in the recipe.
Do not persist initial prompts, print/JSON modes, or fresh/fork flags as resume
arguments. Preserve relevant profile, model, and permission choices through an
explicit tested allowlist. If a custom launch cannot be represented faithfully,
show why automatic recovery is unavailable and retain manual recovery.

The worker is the only live writer. It validates the token, host, containing session,
field bounds, and invocation lifetime. Hooks never rewrite the record directly.
Registration handlers return silently, with no model-visible context or approval
decision. A failed Mesh update must not block the provider's normal operation.

Keep one primary foreground agent recipe per Mesh session in the first version.
A newer registered foreground invocation supersedes the previous primary recipe.
An old invocation's delayed end event cannot clear a newer invocation's binding.
Ignore subagent events for primary selection. Simultaneous background agents and
tools launched in their own detached runtimes require explicit binding or separate
Mesh sessions. Report this limit instead of guessing the intended conversation.

## Provider-specific behavior

### Codex

Capture the authoritative ID from `SessionStart`. Verify that the received ID
reopens the intended interactive conversation, including forks and switched
threads in a shared app-server session. Treat provider IDs as opaque rather than
assuming that every version supplies a UUID or that a session-tree ID equals a
conversation/thread ID. The probe must establish the exact mapping for supported versions.

Resume with `codex resume ID` in the recorded directory and data context. Use an
explicit directory override when needed to avoid an ambiguous cwd-selection prompt.
Never use `--last` for automatic recovery.

Use the normal hook trust flow. Provide a stable installation fragment and a
diagnostic that distinguishes missing, disabled, and untrusted integration.
Do not bypass hook trust or weaken sandbox/approval settings to enable recovery.

Normal TUI exit and provider `SessionEnd` are not assumed to be simultaneous.
The launch helper observes its own foreground invocation ending; hook end events
update only the matching invocation. A crash may skip both and does not erase
the last durable binding.

### Claude Code

Capture identity at `SessionStart`, including startup, resume, clear, and fork.
Compaction must not create a new Mesh task. Update the binding when the provider
actually changes the conversation ID. Ignore subagent starts and stops.

Resume with `claude --resume ID` in the recorded project/worktree and data context.
The native transcript remains authoritative. Preserve a normal exit as closed
work with an explicit Resume conversation action in history.

Do not rely solely on a preassigned `--session-id`: the user can switch or create
another conversation within the same invocation. Use lifecycle observations to
keep the binding accurate. No transcript parser is required for the default path.

## Recovery behavior and failure handling

If the Mesh worker survives, attach to it. Do not start another provider process.
If only the client machine crashed, use T24's exact remote target and keep provider
state on the remote host.

If the worker died, T24 reserves one replacement and this adapter starts the saved
conversation. Exclude additional prompts and automatic continuation instructions.
Reopening history must not replay a previous tool command or inject a new user turn.
Measure provider-specific resume behavior and disclose any existing pending work
that the provider itself resumes.

Keep the original record until a matching provider-start acknowledgement arrives.
A new PTY socket alone proves only that a process started. For a missing hook or
delayed login, leave the new terminal usable and mark conversation recovery unverified.
A retry locates that same replacement rather than launching another process.

If the conversation is deleted, its data root is unavailable, the worktree is
missing, or the provider rejects the ID, retain the record and explain the failure.
Offer Open shell, the provider's session picker, or retry as explicit actions.
Never silently create a fresh conversation or restore into another directory.
Missing executables and expired authentication use the provider's normal setup
path. Mesh does not install tools or copy credentials during recovery.

If the agent exits while the surrounding shell survives, keep the shell as the
default attach target. A separately selected Resume conversation action can reopen
the saved agent in a fresh worker. Preserve the shell's history and directory.
Across Mesh clients, claim each recovery transaction once. If a conversation is
already running outside Mesh and provider APIs cannot prove its ownership, do not
claim global duplicate prevention. State that limitation and require explicit selection.

For older or unsupported provider versions, ordinary terminal use and T24 shell
recovery continue to work. The UI advertises automatic agent recovery only after
a successful identity registration.

## Implementation milestones

1. **Compatibility and association probe.** Retain a tool under
   `scripts/probe-agent-recovery.sh` that records versions, supported commands,
   hook schemas, and sanitized registration events in an isolated directory.
   Prove two simultaneous conversations in the same directory, per-invocation
   routing through a pre-existing Codex daemon, and exact resume. Document the
   supported setup and fallback before building UI around automatic capture.
2. **Shared registration and native launch.** Add provider recipe validation,
   bounded Unix-socket updates, invocation ownership, the launch helper, and
   idempotent setup fragments. Use fake executables to verify stdin, output,
   signals, exit codes, argument quoting, and failure behavior.
3. **Claude capture and recovery.** Implement event mapping and exact resume,
   then test crash, normal exit, clear, fork, data-root override, and changed cwd.
4. **Codex capture and recovery.** Implement the association proven in milestone 1,
   exact resume, trust diagnostics, and shared-daemon cases. Keep unsupported
   modes explicit. Do not expand into transcript scraping to get a green demo.
5. **Unified picker and transport delivery.** Add provider labels and recovery
   status to T24's actions. Cover direct local use, nested remote sessions, and
   SSH. Document setup, storage limits, recovery errors, and supported versions.

Add `scripts/check-t25.sh` with implementation. Most checks must run without an
account, network, or model request. Use protocol fixtures and fake providers that
emit registered identities and report the argv with which they were resumed.

Retain these acceptance cases:

- Two conversations for the same provider in one project recover to their own IDs.
- Codex and Claude used sequentially in one shell never inherit each other's binding.
- A delayed hook from an older invocation cannot overwrite or clear the current one.
- Clear, fork, compaction, and subagents update or preserve the correct primary ID.
- Kill the worker immediately after acknowledged registration. A fresh process
  reads the same recipe and builds the correct resume argv.
- Crash or lose a response at each replacement boundary. Concurrent CLI, WebSocket,
  and SSH requests produce one replacement, including while provider startup waits.
- Missing transcripts, changed roots, missing worktrees, auth prompts, hook failures,
  and unavailable providers preserve both the old record and terminal usability.
- Setup run twice and uninstall preserve unrelated shell and provider hooks.
- Ordinary native agent use outside Mesh is unchanged.

Separately run authenticated smoke sessions for both installed CLIs: create two
distinct conversations, record IDs, terminate their test workers, and recover each
through the picker without a new prompt. Verify the visible conversation and ID.
Record failures as unsupported combinations, not fake-provider successes. The
T24 VM power-loss check remains a separate acceptance step and was not run here.

## Effort boundary

Use native lifecycle hooks and resume commands. Add no SDK, transcript index,
agent UI, model gateway, cloud deployment, account synchronization, or automatic
cross-host migration. No new runtime dependency is planned.

The main uncertainty is Codex invocation-to-conversation association through its
shared daemon. Resolve it first. Basic checkpointing and Claude work can proceed
without hiding that uncertainty. A manual exact-ID binding is the bounded fallback;
a replacement Codex client is outside this plan.
