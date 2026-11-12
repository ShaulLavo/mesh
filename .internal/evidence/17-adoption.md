# Adoption implementation

`mesh update adopt start` verifies its own exact release artifact, signed metadata,
manifest and the full forward and rollback evidence for the actual old daemon and
every retained worker. It then records one canonical `HostAdoption` envelope.
Only that envelope owns confirmation and the retained adoption milestones.

The helper resumes recorded, waiting, activating and confirming phases. Policy is
seeded before helper promotion. Unconfirmed adoption also holds startup config
migration, so the candidate does not rewrite retained host or domain settings.
Manual remains the default through Unit 11.

## Local authority and ordering

The supervisor reads the existing same-account identity and signs local `info`,
`stage` and `grant` controls over the Unix socket. It retains administrator and
device policy. Refusal stops the operation; there is no session bootstrap path.

Required hosts are read through `Client.Evidence` with ordinary device admission.
Both the adoption commit and a later unattended commit must match the operation
and common metadata pin and preserve all workers. The initial last host requires
every other member. Its ordinary later policy requires the selected canaries,
so an offline Mac does not hold subsequent PC updates.

The notice executes the supplied argv without an implicit shell. Its successful
completion is durable, its lead time is observed, and predecessor evidence is
checked again before activation. Tests execute only local fixture notices.

## Inspection, initial helper and migration

`updatebootstrap.InspectInstallation` binds the connected daemon PID to its native
manager and same-account process. It returns settings, the actual configuration
directory, and hashes of service files, arguments and environment. Raw environment
and service values are not put in the operation or evidence replies. Existing
journal cache, mount and timeout settings remain authoritative; an unset process
environment variable does not replace them with a default.

`updateinstall.PrepareAdoptionHelper` is separate from ordinary promotion and
requires unconfirmed canonical approval, no competing journal, a verified side
image, managed prior ownership and a real mapped-process probe after startup.
It takes installation before authority. It supports initial setup and an existing
unjournaled managed helper. Its component preparation checkpoint retains only the
data needed to resume or restore its own helper resources; it is not adoption
confirmation. `PromoteHelper` still requires its finished installation journal.

Package migration prepares a verified old-image copy and an exact executable-only
service change. The last host reaches this step only after its prerequisites and
notice. Worker creation is fenced while the same old daemon is restarted from the
managed path. The old daemon then performs signed staging and produces a genuine
installation journal. Package payloads and identities are not overwritten. Failed
migration restores the original definition and old image and removes only its own
managed copy. Service bytes needed for restoration stay in private local assets,
referenced by hashes rather than exposed through adoption details.

## Validation and limits

Race controls cover durable phase reconstruction, exact input rejection before
controls, policy-before-start, refusal, predecessor and notice holds, rollback,
account binding, and configured system SSH with known-host verification.

The process fixtures use original published v0.1.196, v0.1.248 and v0.1.249
executables. A genuine prior update is followed by one side start, and the actual
promoted helper finishes adoption with worker and shell PIDs and fresh terminal
I/O preserved. Other controls cover absent and existing unjournaled helpers,
owned failure cleanup, a killed start process, failed candidate rollback, package
migration and restoration. Public Sigstore/TUF fixture trust is genuinely signed;
TLS trust is confined to child processes. These use an isolated service-command
controller. Unit 8 owns real systemd and launchd acceptance.

No real fleet rollout, PC update, managing notice, identity/provider change or OS
change is part of these tests. Reproducers, exact heads, run results and actual-host
cleanup are retained in `/work/reports/mesh-auto-update/unit10-adoption/`.
