# Keyed job launch latency (Refs #318)

Status: Approved. Measured 2026-11-22.

## Method

Matched builds of one source tree before and after the change (`mesh_integration` tag, `linux/amd64`),
each run as its own isolated daemon (separate `MESH_STATE_DIR`, `--tailnet-port 0 --ssh-port 0`) that
runs `/bin/true` as a keyed pipe job. Before and after runs alternate, one daemon per run, 120 ms
pause between samples, the first three samples of each run discarded as warm-up. Every sample checks
that the wait completes. The first create after daemon start is kept separately as the cold sample.

Two ways of timing a job:

* Control socket: `execution.create` then `execution.wait`, one connection each (the daemon's cost).
* CLI: `mesh session create --kind pipe` plus `mesh session wait`, two processes (what a caller sees).

The PC could not give usable numbers. During the runs its load average was 35 to 45 and I/O pressure
30 to 50 percent from other agents' builds. A single create took 2 to 7 s there, and several runs hit the
daemon's bounded journal lock wait (`state.unavailable`) on both the before and the after binary. Those
samples are not kept. All numbers below come from the quiet VPS (Debian 13, 2 vCPU, ext4 on a virtio
disk, load under 1), which is also a second, unrelated Linux filesystem. Raw samples are in `vps/`
(`base*` before, `new*` after) and `vps-create-wait/`.

## Result (VPS, milliseconds, p50 / p95 / min)

| What | Before | After | Samples |
| --- | ---: | ---: | ---: |
| Create plus wait over the control socket | 483 / 613 | 165 / 201 | 60 each |
| CLI `session create` plus `session wait` | 624 / 786 | 240 / 286 | 60 each |
| First create after daemon start (cold) | 402 to 559 | 144 to 208 | 3 each |

These are the final numbers, taken after the review round (directory and device barriers always run,
the confirmation memory is bounded and limited to JSON records, Linux-only image stamps) against
current `origin/main` as the "before" build, in `vps/`. The first round, before those changes, measured
472 to 151 ms (control socket) and 557 to 219 ms (CLI) and is kept in `vps-first-round/`; the round
before that (`vps-create-wait/`) gave 477 to 158 ms. The extra 10 to 15 ms is the directory syncs that
are no longer skipped.

For scale, the ssh317 measurements on the PC recorded a warm pooled OpenSSH `/bin/true` at about 6 ms and
the Mesh SSH helper at about 10 ms. A keyed job is a different operation (a durable receipt, a detached
worker process and captured output), so it is not expected to match that, but the gap is now about 20 times
instead of 50 to 80 times.

## Where the time went (VPS trace, one create, milliseconds from request)

| Step | Before | After |
| --- | ---: | ---: |
| Reserve the key and write the receipt | 40 | 28 |
| Dispatch record | 7 | 7 |
| Start the worker process until `Run` begins | 72 | 57 |
| Worker claims the reservation | 12 | 15 |
| Worker opens both stream captures | 25 | 15 |
| Worker hashes its own 62 MB executable | 272 | 0 |
| Child starts, start record and metadata written | 8 | 6 |
| Worker visible, create reply sent | 41 | 10 |
| Create total | 457 | 142 |

Per create and wait the daemon and worker made 457 fsync calls before and 212 after (summed 127 ms and
109 ms on the VPS; the fsync cost is small on a quiet ext4 disk and large on a loaded one). This trace is
from the first round; the final code runs the directory syncs on every confirmation again, so its fsync
count is higher than 212.

## What changed

1. Workers no longer hash their executable. The daemon hands over the digest it already holds together
   with a stamp (device, inode, size, mtime, change time) of the file, and the worker trusts it only while
   its own executable carries that stamp and the digest is a well-formed SHA-256. Only Linux, which pins
   the running image through `/proc/self/exe`, hands a digest over. This was the single largest cost, and
   it hit every session start, not only keyed jobs.
2. A JSON record this process already synced is not fsynced again while its file is unchanged. Directory
   and device barriers still run on every confirmation, anchors and locks are never remembered, and the
   memory is bounded and forgets removed paths. Every journal read used to fsync the file, then each parent
   directory, then the file again.
3. Admission counted active receipts by reading and confirming every retained receipt, and the output
   retention check observed every retained receipt on each create. Admission no longer pays the
   confirmation (it still decodes the retained receipts), and the retention check does nothing below the
   active limit.
4. On Linux the final device flush repeated what the data fsync and the directory fsyncs had just done, so
   it is skipped there. Darwin keeps its full flush.
5. The worker opens its two stream captures together and writes the start record and the metadata
   together. Nothing becomes visible to the daemon until both are durable.
6. Readiness and wait polling start at 1 to 2 ms and back off, instead of fixed 20 ms and 100 ms steps.

## What remains

On the VPS a create is about 140 ms: roughly 60 ms to start the worker process (Go runtime start of a 62 MB
binary plus the daemon's fork), about 70 ms of durable writes (17 records, several of them in the
worker before the child can start), and the rest in the control round trips. A new worker process plus a
durable receipt is the floor of the current design. Getting to SSH's tens of milliseconds would need a
pre-started worker or fewer records per job, and invariants 2, 3 and 5 constrain both; that is a design
decision, not a waste to remove.
