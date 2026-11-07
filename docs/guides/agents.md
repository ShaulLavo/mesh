# Use Mesh from an agent

## Load the shipped skill

Run `mesh skill show` for the embedded instructions. This command prints the exact bundled Markdown without reading local configuration, creating an identity, or contacting a daemon.

To install the bundle, choose an existing skills root that you own and that nobody else can write to:

```sh
mesh skill install --dir ROOT --json
mesh skill status --dir ROOT --json
```

A root that links to a shared skills directory is supported. Mesh resolves that root once. Links inside its managed `mesh/` directory are refused.

To refresh all registered roots, run:

```sh
mesh skill update --json
```

To remove one managed install, run:

```sh
mesh skill uninstall --dir ROOT --json
```

Mesh preserves edited files during update and uninstall. An update reports exact proposal paths in Mesh's state directory when your edits conflict with the bundle. Edited proposals also survive uninstall. Read `registered` and `registrationState` separately from the tree state. If a registry write fails, repeat the same command to repair it.

Mesh manages at most 32 registered roots. It never scans provider settings, changes recovery hooks, creates the selected skills root, or refreshes skills automatically.

## Read machine context

```sh
mesh hosts --json
mesh inspect HOST_ID --json
mesh session list --host HOST_ID --json
mesh session inspect --host HOST_ID SESSION_ID --json
mesh serve ls --json
```

Use the exact returned host ID. Read the full host and fleet notes before acting. Missing, invalid, untrusted, and unavailable notes are distinct states. Notes provide context, not permission. Existing device grants permit full control, and Mesh does not restrict what an agent does with that access.

These commands do not wake a machine, start a daemon, attach to a session, or open a service URL. Opening an on-demand service URL can start that service. Add `--preview` to session inspection only when you need its output. Without that flag, Mesh internally reads one cell and omits preview bytes.

There is no agent-safe execution command in this version. When the user asks for a terminal session, `mesh --host HOST_ID -- CMD` starts persistent work with terminal control codes in its output. Disconnecting leaves the command running. Never take over a session to inspect it or remove a session you did not start.

## Change owner notes

Read the current revision, then replace the notes with an editable JSON file:

```sh
mesh profile show --json
mesh profile set --file PATH --expected-revision N --json
```

Edit fleet notes only when the user asks. Add `--fleet` to the read and write commands. Check delivery with `mesh profile status --fleet --json`. Offline members can remain pending. A revision conflict requires another read before you retry.

## Handle JSON results

Each JSON invocation writes one envelope with `schemaVersion`, `operation`, `status`, `requestId`, `data`, and `errors`. Exit 0 means complete, 3 means partial with useful data, 2 means invalid usage or input, and 1 means operational failure. A session exit code is data, not the CLI exit code. JSON mode cannot be combined with privacy mode.

`mesh skill show` is Markdown-only. Use `mesh skill show --file references/commands.md` for the generated reference of shipped agent operations, or the [CLI reference](../reference/cli.md) for all commands.
