# Session contract and deployment boundaries

Status: Approved, 2026-10-08. Plan 336 Track H transfers the remaining issue discussions into this plan. Execution begins with source review and isolated evidence. This approval covers investigation and a concrete execution sequence, not a transport rewrite, repository split, feature removal, or live deployment change.

## Scope and current evidence

Reviewed Mesh main `9b7a47cfc05e349e42e816cc72720a1787d138f3`, v0.1.208. Preserve the six invariants in `CLAUDE.md` and the decisions in [01-decisions.md](01-decisions.md). Workers own PTYs; connections are disposable; a host reboot interrupts a process rather than resurrecting it.

This plan owns [#240](https://github.com/ShaulLavo/mesh/issues/240), [#241](https://github.com/ShaulLavo/mesh/issues/241), [#242](https://github.com/ShaulLavo/mesh/issues/242), [#243](https://github.com/ShaulLavo/mesh/issues/243), [#244](https://github.com/ShaulLavo/mesh/issues/244), and [#245](https://github.com/ShaulLavo/mesh/issues/245). The original discussions and their limitations remain evidence. No performance comparison or exploitable vulnerability was established by filing them.

Related execution homes:

- [Plan 07](07-quality-and-security-wave.md#historical-fixture-failures) retains historical fixture failures separately. Passing controls do not establish their causes.
- [Plan 09](09-residual-hotspots.md#transport-comparison-baseline) owns #246's comparative measurements. Measurements and the compatibility corpus below gate interaction experiments.
- [Fregat Plan 290](https://github.com/ShaulLavo/fregat/blob/main/plans/290-mesh-device-authorization.md) owns device authorization. Native control already uses TLS 1.3, pinned device keys and grants in `internal/transport/auth.go`; its custom verifier must be assessed as a whole.
- [Fregat Plan 291](https://github.com/ShaulLavo/fregat/blob/main/plans/291-mesh-private-services.md) owns retiring public temporary-app sharing. Ordinary explicitly named public services remain supported.
- [Fregat Plan 292](https://github.com/ShaulLavo/fregat/blob/main/plans/292-mesh-zerotier.md) owns the approved alternate-network integration. This investigation does not replace its contract.
- [Fregat Plan 296](https://github.com/ShaulLavo/fregat/blob/main/plans/296-mesh-update-recovery.md) owns remaining helper activation readiness and Pi idle-CPU verification.
- [Fregat Plan 302](https://github.com/ShaulLavo/fregat/blob/main/plans/302-mesh-private-app-observability.md) owns private-app output, startup phases and caller-side URL lookup warnings.

## Product and dependency map

- [ ] Resolve #240 with a short product charter, non-goals, and a keep / optional integration / deployment-policy map. Evaluate the working definition "durable terminal sessions on your machines, accessible from whichever device you use" before publishing it as a promise.
- [ ] For #241, map the dependencies of session creation and attachment separately from discovery, provisioning, serving, DNS, metrics, power and application recovery. `internal/daemon/app.go` composes them; `internal/dnsname/records.go:15` and `internal/apps/types.go:23` still contain `shaulavo.dev` at the inspected revision. Determine which values are deployment policy and which separations need configuration, packages or genuinely different credentials.
- [ ] Propose the smallest behavior-preserving extraction sequence, with retained-worker compatibility and an explicit profile for the existing deployment. Keep generic power and dashboard capabilities where useful. Avoid a plugin framework or a network replacement merely to look generic.
- [ ] Verify the minimal contract using two isolated machines when separately authorized. Create a terminal, disconnect, attach from a fresh client, and restart the coordinator while the worker survives. Require no personal DNS, TV setup or AI-provider credentials. A documented Tailscale prerequisite is compatible with this check.

## Persistence and terminal fidelity

- [ ] For #242, publish a failure/guarantee matrix for network loss, client exit/crash, takeover, coordinator restart/upgrade, worker death and host reboot. Track process identity, session ID, screen, output, directory, history and application conversation independently. Mark each cell verified, intended or unverified.
- [ ] Derive focused tests and user terminology from that matrix. Cover local, native remote and SSH attachment, nested clients and mixed worker/client versions. Reconstructed shells and conversations must be distinguished from surviving processes.
- [ ] For #244, build a reusable compatibility corpus for alternate screens, resize, partial escape sequences, Unicode/wide/combining characters, paste, queries, hyperlinks, titles and client changes. Define expected lossy or unsupported behavior explicitly. `internal/terminal/screen.go` is a passive snapshot renderer; the attached terminal answers live queries. This differs from Mosh's ongoing terminal-state synchronization.
- [ ] Document who answers queries while detached and how capabilities change on takeover. Test live forwarding separately from structured preview sanitization.

### Clipboard routing

[#124](https://github.com/ShaulLavo/mesh/issues/124) remains an unconfirmed client-transition report. Its inspected Codex 0.159.2 uses native clipboard copying first and enables OSC 52 for SSH/tmux; the reported worker had a Linux display but no SSH/tmux environment. Mesh forwards output in `internal/cli/attach.go`. This is a plausible routing explanation, not an end-to-end reproduction.

- [ ] Begin with a harmless marker and a fixture that proves OSC 52 reaches the active terminal. With separate authorization for native UI and a real provider, compare the reported cmux 0.64.25 / Codex 0.159.2 case, terminal selection, and a launch without a native host clipboard. Read only the marker, never unrelated clipboard contents.
- [ ] Cover a locally created persistent session later attached remotely. Decide clipboard delivery based on the current attachment contract, not fabricated SSH environment. Add the demonstrated transition to the compatibility corpus before changing worker environment or provider integration.

## Interaction and transport diagnosis

- [ ] For #243, compare display freshness, input acknowledgment, server progress and restoration as separate facts. Inspect `internal/transport/client.go`, `internal/worker/serve.go` and `internal/daemon/state_server.go` for byte-offset replay, snapshots, connection generations and uncertain writes.
- [ ] Pin the Mosh revision before researching adaptive sending, ordered input, display synchronization and acknowledgment UX. Review its license before copying any code.
- [ ] Rank bounded experiments using Plan 09's comparison baseline and the fidelity corpus. Current-screen resynchronization needs a valid snapshot and exact output boundary. Keep historical-output retrieval separate. Avoid arbitrary ANSI dropping and automatic replay of uncertain input. Worker acknowledgment cannot promise exactly-once command execution across crashes.
- [ ] Consider predictive echo only after measured latency benefit and explicit no-echo/password and full-screen correction tests. No UDP, QUIC, compression or transport rewrite is selected.

### Historical HTTP 426

[#188](https://github.com/ShaulLavo/mesh/issues/188) reported v0.1.168 `serve ls` returning local healthy routes plus a remote HTTP 426 handshake error. The historical remote version was never recorded. [PR #255](https://github.com/ShaulLavo/mesh/pull/255) shipped an explicit marked control-authentication diagnostic and bounded reconnect refusal in v0.1.206. Current `internal/transport/auth_diagnostic_test.go` preserves unrelated failures. That repair does not identify the historical remote cause.

- [ ] Preserve the shipped diagnostic regressions. On a genuine recurrence, record sanitized source/destination versions, authentication mode and response marker before classifying it as legacy-server upgrade, current authentication refusal or another HTTP failure. Reproduce the observed class with local fixtures before changing behavior. No live peer enrollment or grant change is authorized here.

## Authority and service health

- [ ] For #245, produce a principal/capability map for observation, terminal input, process creation/kill, update administration and publishing. Show the OS-account authority behind each operation. Keep read-only observers and update administrators distinct from full terminal control.
- [ ] Verify enrollment, changed-key refusal, revocation and retirement across native control and SSH, including retained workers and already-admitted work. Reuse Plan 290's controls. Test fixtures first; no credential, grant or public-exposure changes follow from this plan alone.
- [ ] Assess whether public-serving separation creates a real permission boundary. An extra process with the same powerful credentials is insufficient. Rank demonstrated defects separately from hardening opportunities.
- [ ] Correct documentation drift against current source. `docs/tasks/T05-websocket-transport.md` and item 14 in `docs/plan/02-status.md` retain the earlier Tailscale-only description, while `internal/transport/auth.go` implements pinned-key TLS 1.3. Update historical wording without claiming that all audit questions are resolved.

### Public route reachability

[#126](https://github.com/ShaulLavo/mesh/issues/126) reported v0.1.139 accepting a healthy public static route while public DNS returned a tailnet address and TLS served the private wildcard certificate. The private route returned HTTP 200. The public route was withdrawn. This distinguishes app health from public reachability but does not identify the configuration cause. Plan 291 preserves ordinary public services, so this finding survives removal of public temporary apps.

- [ ] Define separate origin readiness, route registration, DNS reachability and TLS validation observations for ordinary public services. Use local resolver, TLS and edge fixtures to reproduce the wrong-address/wrong-certificate combination. Do not probe or repair the old live route without authorization.
- [ ] Decide the narrow warning or status correction after that reproduction. Verify private routes remain healthy and a public hostname is never called externally reachable merely because its origin responds. Preserve the public-edge TLS contract in `docs/tasks/T13-public-edge.md`.

### External Gallery listener

[#235](https://github.com/ShaulLavo/mesh/issues/235) reported Gallery connection refusal at loopback port 8190 after a host restart on v0.1.196. Its latest comment confirms no upstream listener, while Mesh's dashboard accurately reports refusal. No Mesh defect or Gallery startup cause was established.

- [ ] Ask the service's owning execution lane to determine whether Gallery is intentionally off and what owns its boot activation. Keep this deployment observation out of terminal-core fixes. With separate authorization, verify only that declared upstream's expected lifecycle; preserve Comfy data and configuration.
- [ ] Close the plan item as expected downtime, an external service repair, or a demonstrated Mesh activation defect with its reproduction. A healthy dashboard does not establish upstream availability.

## Delivery and acceptance

Each section produces its named matrix, map, corpus or bounded experiment with exact source versions and retained evidence. Preserve the issue-specific uncertainty; none of these transfers is a causal-fix claim. Implement only demonstrated small units in separate PRs, with fail-first tests and the affected Mesh gates. Leave live installations, owner sessions and provider accounts untouched until independently authorized.

Completion requires a tested minimal-install contract, explicit lifecycle and fidelity limits, a current capability map, and evidence-backed interaction decisions. Historical observations can remain pending recurrence in this plan after their tracker entries close. Do not restart a broad reproduction campaign solely because an issue was transferred here.
