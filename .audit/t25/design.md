# T25 design

## Usage

Opt in with `mesh shell-init bash --agents` and install stable provider hooks with
`mesh agent setup PROVIDER --install`. Run the ordinary provider command inside
Mesh. Select Resume conversation after interruption, or explicitly bind an exact
native ID when automatic association is unavailable.

## Grounding

The worker owns one PTY and recovery.Record. checkpoint submits to recovery.Writer, which fsyncs before acknowledging. recovery.Recover serializes source requests, freezes one replacement intent, and reconciles the reserved ID across failures. Its existing completion condition is published worker metadata. CLI, SSH and WebSocket clients already share this transaction.

## Chosen shape

A small agentresume package owns bounded provider fields, event decoding, supported launch options and native resume argv. The CLI supervises the native foreground process and holds a worker registration lease. Stable hooks report only recovery identity over a separate bounded local request. The worker validates ownership and serializes changes through durable acknowledgement. Closing a lease unexpectedly invalidates future events but preserves crash recovery state.

The frozen replacement plan contains an agent recipe. The replacement worker knows the expected identity. A matching start event stores an immutable resume receipt alongside the current recipe. Later clear/fork events can change the current conversation without erasing that receipt. A published PTY with no matching receipt remains usable and unverified; retries return it.

## Synthesis

The worker-owned domain and existing transaction are the base from worker_design. The launch-scoped lease comes from cli_design and avoids guessing invocation lifetime from recycled PIDs. Hook-written files and generic command restart recipes were rejected because they cannot prove durable identity ownership and resume completion. A new provider client was rejected because the feature must preserve native terminal interfaces.

## Boundaries

Provider data stays on the owning host. Mesh stores no prompts, transcripts, secrets or full environments. Automatic capture is unavailable for a setup whose invocation route cannot be proved. A manually supplied ID alone is not verified identity. Native hook trust stays in the provider's normal flow.

## Validation

Use fake provider processes for deterministic stream, signal, argv, lifecycle and crash tests. Retain native probe evidence separately; fake-provider passes do not prove native association or authenticated resume.

## Final revisions

Agent claims use the source invocation token so a closed conversation can resume
without consuming the surviving shell's future recovery claim. A replacement
reference points to that exact frozen claim. Canonical shell claims remain the
catalog target after a closed agent's surrounding shell crashes.

Read-only Codex binding records lookup provenance. It can save an explicit source
identity but cannot produce a resumed-provider receipt. Claude binding uses native
resume and its matching hook. The replacement can show its native terminal before
confirmation and remains unverified until the expected start event arrives.

Actual containment replies establish worker liveness. A connected Unix socket
alone could outlive a killed worker through inherited descriptors. Ambiguous
timeouts and malformed responses prevent new launches. Closed connections allow
interrupted-session recovery.
