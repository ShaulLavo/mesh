# T18 implementation

- [x] Ground edge publication, SSH handling, identity and control boundaries.
- [x] Compare and synthesize two interface sketches.
- [x] Implement signed mutations, transactional outbox and reservations.
- [x] Implement token-bound SSH activation and HTTP proxying.
- [x] Wire CLI, daemon and local recovery.
- [x] Exercise acceptance cases and run repository checks.

Grounding: Controller.register holds commitGate across durable T13 apply and Registry.Replace. Registry.ServeHTTP validates public host and terminal exclusion before stable client rate/concurrency and global upstream quotas. storage.ApplyEdgeSnapshot rechecks collisions transactionally, currently only by (hostname,path). sshd installs public-key auth from a safely opened authorized_keys; Charm dispatches global requests serially per connection and puts *ssh.ServerConn on its context. ForwardedTCPHandler's wire structs are reusable, but its listener implementation is unsuitable. daemon marks Unix connections with localClientKey, never WebSocket connections. Identity IDs are raw-url-base64 Ed25519 public keys. Existing T16 work in the checkout is preserved.

Synthesis: retain Controller.commitGate. Moving it into Registry adds a second migration of T13 failure/reconciliation paths without hiding more from callers. Use separate in-memory tunnel publication so T13 Replace cannot discard live SSH routes, while sharing HTTP ingress and concurrency limits. Adopt token-capturing cleanup and explicit SSH window accounting from the registry candidate. Outbox returns exact conclusive refusal receipts so rejected creates cannot block owner releases; ambiguous errors have no receipt. Implementation contracts are in contracts.md.

Verification: full `go test -race ./...`, `go vet ./...`, `go mod tidy -diff`, and all 37 scripts in `scripts/verify.sh` passed. The focused T18 script and final diff check also passed. Final channel-retirement and concurrent-cleanup regressions pass under the race detector. One full integration rerun hit a legacy recursive SCP failure in the existing non-edge fixture; its standalone recheck and the next full 37-script run passed. No SCP source was changed for T18.

Self-review: reproduced an upload timeout/upstream disconnect race returning HTTP 503 instead of 408, then fixed the tunnel error handler to retain the inbound timeout marker. The regression fails before the fix and passes afterward. Prepared a separate checkout against main’s existing SSH dependencies; T18 needs no dependency migration. The final T18-only checkout passed `go mod tidy -diff`, full `go test -race ./...`, `go vet ./...`, and all 36 integration scripts. The earlier 37-script runs included local T16/SFTP work, which is excluded from this commit.
