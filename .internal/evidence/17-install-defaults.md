# Managed installation defaults

Status: Approved, implementation built. Independent review and the native
installed release gates remain with the parent coordinator.

Unit 11 owns the root installer, embedded Linux and macOS installers, local
daemon installation, the missing-policy default, and focused tests. It consumes
policy source `6a10490b` and adoption source
`1ce24815941e12c56c9dcb5565f20616abeceeb5` through ordinary merges.

A fresh managed official installation must verify and start its helper before
the daemon starts. Its absent policy means automatic with no predecessors.
Persisted decisions and policy revisions remain authoritative. Package,
client-only, and development installations retain their separate eligibility.

The embedded installers and root installer run `daemon prepare-updates` before
daemon activation. They pass the daemon's service kind, launchd domain when
needed, and state directory. A stable managed executable authenticates its
release through `OfficialHostSource`, then calls `EnsureInstalledHelper` with
the authenticated manifest and the native manager and mapped-image probes.
Development builds leave the updater unconfigured.

`EnsureInstalledHelper` keeps the finished-journal guard on ordinary promotion.
Its fresh path has a separate durable preparation checkpoint and refuses a
competing adoption or an unmanaged helper. Failed preparation restores owned
resources. An unverified live PID holds cleanup. Retrying the same verified
installation resumes its checkpoint. Existing newer helpers remain intact, and
an older unjournaled helper still requires adoption.

Before choosing a missing-policy default, the daemon verifies its actual native
service, PID, invocation, and environment with `InspectCurrentDaemonService`.
Generic command stores remain manual. Reading a default never writes policy or
enrollment files. Revision zero identifies the absent policy, and `policy off`
advances the revision and fences an initial automatic request.

The focused tests cover the initial automatic hop, revision-zero fencing,
persisted manual decisions, installer order, helper readiness, rejection,
rollback, retry, and client-only and development controls. Manager command
fixtures are isolated unit tests. They do not satisfy the native release gate.

The activation release requires normal signed publishing CI and Unit 8's real
native installed-pair and chain gates. Local implementation checks do not
authorize publication or a real fleet change.
