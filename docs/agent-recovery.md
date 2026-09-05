# Recover an exact Codex or Claude conversation

Enable provider hooks and run the provider through Mesh to save its conversation
ID. After a crash, select **Resume conversation** in the picker or run
`mesh recover SESSION --agent`. Mesh opens the provider's native terminal interface
in the recorded project directory with its exact saved ID.

The tested versions are Claude Code 2.1.261 and Codex CLI 0.153.4 with its direct
TUI. Check the [compatibility report](agent-recovery-compatibility.md) for observed
behavior and limits. Provider histories and authentication must remain available
on the original host.

## Install the provider hooks

Run the setup command for the providers you use:

```sh
mesh agent setup claude --install
mesh agent setup codex --install
```

Setup merges stable Mesh commands into Claude's `settings.json` or Codex's
`hooks.json`, preserving unrelated hooks and settings. It uses `CLAUDE_CONFIG_DIR`
or `CODEX_HOME` when set. Otherwise it uses each provider's directory under your
home. Use `--path /absolute/settings-file` to choose a different file, or omit
`--install` to print the fragment without editing settings.
Existing settings symlinks remain intact. Setup updates their target and reports
an error if that target is missing.

For Codex, open its normal `/hooks` screen and review and trust the Mesh hooks.
Keep the Mesh executable at the installed path. If that path changes, install the
fragment from the new executable and review the changed hook definition.

Check the selected installation:

```sh
mesh agent doctor claude
mesh agent doctor codex
```

The diagnostic checks provider version and hook configuration. Codex's own hook
screen remains the authority for trust. Hook configuration alone does not prove
that the containing Mesh worker acknowledged an identity.

## Save conversations during normal terminal use

In a Mesh terminal, run a provider through the launch helper:

```sh
mesh agent claude --
mesh agent codex --
```

Pass normal provider arguments after `--`, for example
`mesh agent claude -- --model sonnet`. Mesh preserves supported model and
permission choices in the saved recipe. It passes the original arguments to the
native provider and does not save an initial prompt as a resume argument.

To keep using the commands `claude` and `codex`, add the relevant line to your
shell startup file:

For Bash:

```sh
eval "$(mesh shell-init bash --agents)"
```

For Zsh:

```sh
eval "$(mesh shell-init zsh --agents)"
```

The opt-in snippet also saves shell directories and history for ordinary workspace
recovery. It preserves existing functions named `codex` or `claude`. Use the
explicit launch helper if your own function already uses either name.

A provider outside a containing Mesh terminal runs normally. Inside Mesh,
unsupported versions or launch options print a recovery diagnostic and still run
the native command. Print modes, custom settings blobs, alternate backends, and
Codex `--remote` are outside automatic capture. A conversation becomes recoverable
only after the worker durably acknowledges its provider identity.
Claude backend environment switches and custom API endpoint overrides also
disable capture because the recovery recipe does not preserve their routing.

Use terminal Ctrl+C or Mesh session signals to interrupt the provider. Sending
`SIGINT` or `SIGQUIT` directly to the `mesh agent` helper PID does not forward that
signal to the provider. Target the provider process or its foreground process
group when sending these signals outside Mesh.

## Bind an existing conversation explicitly

In the intended Mesh terminal, use an exact ID from the provider's native session
interface:

```sh
mesh agent bind claude CONVERSATION_ID
mesh agent bind codex CONVERSATION_ID
```

Claude binding opens that conversation with native resume and waits for its
matching startup hook. Codex binding uses a read-only native lookup to validate
the exact ID and directory, then saves the binding without opening a TUI.
Binding does not choose the latest conversation or infer one from the directory.

Use separate Mesh terminals for simultaneous agents. Mesh retains one primary
foreground conversation per terminal. A newer registered invocation replaces that
terminal's primary binding. A delayed event from an older invocation cannot
replace or clear the newer binding.

## Resume after interruption

Select the interrupted task in the picker and press `a` for **Resume conversation**.
The compact window picker labels this key **conversation**.
An active saved agent recipe also makes conversation resume the default recovery
action. To select the action explicitly by session ID, run:

```sh
mesh recover SESSION --agent
```

Mesh retains the old record and creates a replacement terminal. Repeated recovery
requests reconnect to the same replacement. A matching native provider hook marks
conversation recovery as verified. Claude's tested version delivers that hook on
resume. Codex's tested direct TUI restores the conversation but emits no hook while
idle, so its recovery remains unverified until the provider reports the saved ID.
If that replacement crashes before confirmation, recovery reuses the exact
pending plan. The picker keeps the conversation action available.

To open a shell instead, press `s` in the picker or run:

```sh
mesh recover SESSION --shell
```

If an installation, saved data root, project directory, authentication, or provider
history is unavailable, restore that dependency on the original host and retry.
The old recovery record remains available. If the launched provider exits with an
error, the recovery terminal offers to open a shell in the saved directory.
You can launch and capture a new conversation from that shell. If the shell
crashes before a new conversation is registered, default recovery retries the
original conversation. Use `--shell` to recover the shell explicitly.

## Remove the integration

Remove the shell initialization line from your shell startup file, then remove
the provider hooks:

```sh
mesh agent setup claude --uninstall
mesh agent setup codex --uninstall
```

Uninstall preserves unrelated provider settings and hooks. It does not delete
provider histories or existing Mesh recovery records. See
[workspace recovery](recovery.md) for previous output, older attempts, and host
recovery, and the [compatibility report](agent-recovery-compatibility.md) for the
native probe and unverified lifecycle cases.
