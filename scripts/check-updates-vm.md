# Check updater behavior in a disposable VM

`check-updates-vm.py` runs real user-systemd updates, checks both worker and shell process identities, reboots at a persisted activation grant, and verifies completion and interrupted-session reporting. Add `--recover` to prove a saved checkpoint opens a new shell that accepts input.

The script uses an operator-provided SSH VM. It does not download a VM image, install a hypervisor, copy credentials, or change the host's trust store. It requires Python 3 and OpenSSH on the operator's machine, and Python 3, systemd, and passwordless sudo in the guest. Use a separate, disposable VM: the test suspends its helper and reboots the entire guest.

Prepare the guest:

1. Enable lingering for its test account and run Mesh through that account's `mesh.service`, with `KillMode=process`. Custom service flags and drop-ins are allowed; the script records their hashes.
2. Start a shell session and detach. Supply its ID to `--session`. For `--recover`, start it with a build that writes recovery checkpoints.
3. Place the new CLI at a separate absolute path. Supply the actual installed daemon executable to `--installed`.
4. Serve two exact test releases through guest-local HTTPS at the ordinary `https://github.com/ShaulLavo/mesh/releases/download/VERSION/` paths. Map `github.com` to a guest loopback address and trust the fixture CA only inside the guest. The script refuses a non-loopback release host. Supply valid manifests, archive/binary SHA-256 values, and exact compatibility transitions produced by `prove-release-transition.sh`. Keep the HTTPS fixture running across reboot.
5. Choose an initial release whose helper contains the changes being tested, followed by a second release for the interrupted update. The first update must have a tested transition from the installed binary, and the second from the first.

Example, using an SSH alias and operator-owned SSH configuration:

```sh
python3 scripts/check-updates-vm.py \
  --disposable-vm --target mesh-test-guest --ssh-config /path/to/ssh-config \
  --state-dir /home/test/mesh-state \
  --cli /home/test/new-mesh --installed /home/test/bin/mesh \
  --session ABCD --update-version v9.0.1 --reboot-version v9.0.2 \
  --recover --output /work/tmp/mesh-vm-acceptance
```

The output directory contains before/after process and service snapshots, terminal I/O evidence, the pre-reboot Granted journal, the post-reboot Committed receipt, fleet status, and `result.json`. A successful run verifies guest architecture rather than assuming the operator's architecture. An interrupted process is reported as interrupted; recovery creates a new shell and does not restore arbitrary process memory.

This acceptance test covers the Granted reboot boundary. Subprocess fault tests remain necessary for the other journal boundaries, and Linux VM results do not establish launchd behavior.
