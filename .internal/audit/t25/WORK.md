# T25 implementation

- [x] Ground: trace recovery and verify native provider association.
- [x] Sketch: compare worker and CLI integration designs.
- [x] Agree: synthesize the design and proceed under the user's implementation request.
- [x] Implement: registration, launch, recovery, setup, picker and transport coverage.
- [x] Scrap: review assumptions against implementation and revise where necessary.
- [x] Verify: focused real-process checks, full Go and integration checks, native provider evidence.

Build and probe artifacts live under /work/tmp/mesh-t25 and /work/cache/go-build.

Native evidence is retained in docs/evidence/t25-native-2026-09-05.json. Native
completed conversations resume by exact ID. Claude confirms the resumed identity.
Codex direct resume remains unverified while idle, and shared app-server capture
is disabled because the server supplies the hook environment.

Review corrections include per-invocation claims for agents that finish while the
shell survives, a separate canonical shell claim, bounded actual worker replies
for liveness, and lookup-only registration that cannot mint a resume receipt.
The final launch parser also bounds total option bytes and rejects relative
additional-directory options whose meaning would change after restoring cwd.

Real provider hooks exposed legacy terminal color queries before main. Replacing
Wish middleware with the underlying SSH server removed that startup delay while
preserving logging, panic handling, and bounded per-IP rate limiting. This also
exposed older integration fixtures that observed metadata before the worker had
saved its exited or detached state. They now wait for that durable observation.
The SSH fixture also distinguishes a saved prompt in the picker preview from the
new shell prompt by waiting until the picker releases the terminal.

Final verification passed on 2026-09-05: all 35 integration scripts, full Go race
tests and vet, the focused check-t25 script, Linux amd64/arm64 and Darwin arm64
cross-builds, module diff, formatting, and whitespace checks. Full unlimited lint
retains only the same eleven prior findings. Logs are in /work/tmp/mesh-t25.

The requested pre-push review is recorded in [review.md](review.md). Its regression
and verification logs are in /work/tmp/mesh-t25-review.
