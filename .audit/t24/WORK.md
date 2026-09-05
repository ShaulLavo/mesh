# T24 working contract

Read CLAUDE.md, docs/tasks/T24-crash-recovery.md, docs/plan/00-overview.md and relevant existing code before editing. The worktree already contains T19 changes; preserve them. No new dependencies. No commits. Keep code shallow, errors contextual, and wire input bounded. Worker owns live recovery.json; transaction files are separate; SQLite remains daemon-owned. Retain previous attempts and never automatically execute arbitrary saved commands. Use isolated state dirs for tests. Run focused tests for your files and report exact commands/results. Do not edit another agent's assigned files without coordinating.

## Phases
- [complete] Ground existing lifecycle and compare two recovery API sketches.
- [complete] Synthesize API and checkpoint storage/writer.
- [complete] Shell hooks and shared restart transaction.
- [complete] CLI, SSH, picker and exact remote continuation.
- [complete] Integration checks, documentation, and cross-model review. VM power-loss acceptance and the full process-crash boundary matrix remain documented operator/stress checks.

## Initial grounding
Worker Run owns PTY and meta.json. pump feeds replay ring and terminal under w.mu; inspection obtains OS process info outside that lock. launch.go reserves a new directory with the .launching marker before spawning. Window and SSH currently create then forget an interrupted source independently. Catalog reconciles socket/boot metadata. Existing nested registrations have exact host/session identities but vanish on connection death. The new persistence must not resurrect those live registrations.

## Publish authorization

The user subsequently requested a push. Commit and push the independently staged T24 implementation; preserve unrelated T19 edits in the working tree. Validate the exact isolated snapshot before publishing.
