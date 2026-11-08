# Terminal fixture cleanup diagnostics

A failed worker completion exchange in `terminal_window.Fixture` emits one
`fixture.worker_failure` record and keeps `failure-worker-<session>.json` in the
fixture root. Successful exchanges emit nothing and create no evidence file.

The observer pins the connected worker with a pidfd, verifies its executable and
private session directory, and records a child relationship only when the live
child's parent is that worker. It never signals a PID taken from metadata alone.

After the original failure, SIGQUIT asks Go's existing runtime to dump stacks to
`worker.log` and ends that already-failed fixture worker. This is destructive
failure diagnosis, not a recovery or a second kill attempt. The original exception
still fails the test. Fixture-owned descendants remain the existing owner's
cleanup responsibility.

The read is capped at 128 KiB. The emitted summary includes only goroutines with
worker frames, at most 160 lines of 256 characters. Runtime arguments, registers
and absolute build paths are omitted. Waiting for the dump uses at most 200 ms
of the original cleanup allowance, never a new allowance. Evidence can be partial
or unavailable if the worker has already exited or the allowance is exhausted.
A missing live child is not proof that the worker observed or reaped its exit.

`integration/window_entry.sh` first proves this in a real window worker.
It gives an HUP-ignoring shell a deliberately short control exchange, checks the
original timeout and useful handler, child-wait and PTY-pump stacks, and verifies
silent successful cleanup. If coverage fails, the entry stops before the ordinary
window scenario. Its existing 60-second CI allowance is unchanged. This control
does not reproduce the historical CI bug.
