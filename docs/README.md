# Mesh documentation

Mesh runs terminal sessions on your Tailscale-connected machines and lets you open them from any other machine. Start with the [README](../README.md) for the pitch and install steps.

## Start here

- [Getting started](getting-started.md): run a session, detach, come back, then add a second machine.
- [Troubleshooting](troubleshooting.md): the failures you are most likely to hit, with their exact messages.

## Guides

Task-focused. Each assumes Mesh is installed.

**Sessions and terminals**

- [Run a command that ends with its connection](guides/run-without-a-session.md): `mesh ssh`.
- [Use Mesh for every terminal window](guides/terminals.md): `mesh --window`.
- [Copy files between machines](guides/copy-files.md): `mesh cp`.
- [Reach sessions over SSH](guides/remote-access.md): phones and machines without Mesh.

**Recovery**

- [Recover a workspace](guides/recovery.md): directories, history and screens after a reboot.
- [Recover an exact Codex or Claude conversation](guides/agent-recovery.md)
- [Hibernate idle agent sessions](guides/hibernation.md)
- [Verify recovery after a VM power cut](guides/recovery-powerloss.md)

**Your fleet**

- [Dashboard](guides/dashboard.md): usage, sessions and services for every machine.
- [Approve a device](guides/approve-devices.md)
- [Approve a device on selected pinned hosts](guides/device-enrollment.md)
- [Share connections between personal devices](guides/fleet-sharing.md)
- [Update Mesh](guides/updates.md)
- [Wake a machine](guides/power.md)
- [Privacy mode](guides/privacy.md): mask personal data for recordings.
- [Run Mesh on macOS](guides/macos.md)

**Serving**

- [Serve a dev server on demand](guides/serve-on-demand.md)
- [Temporary apps](guides/temporary-apps.md)

## Concepts

- [Architecture](concepts/architecture.md): daemon, workers, transports, and how a session survives a dropped connection.
- [Device access](concepts/device-access.md): how a host decides who may control it.

## Reference

- [CLI reference](reference/cli.md): every command's `--help`, generated.
- [Files, ports and environment](reference/files-and-environment.md)
- [Machine names](reference/machine-names.md)
- [Deployment domains](reference/deployment-domains.md): `domains.json` and private certificates.
- [Agent recovery compatibility](reference/agent-recovery-compatibility.md): probed Codex and Claude versions.

## Decisions

[Architecture decision records](adr/README.md) explain why the code is shaped the way it is.

## Contributing

[Development](development.md) covers tools, gates, hooks and the integration suite.
