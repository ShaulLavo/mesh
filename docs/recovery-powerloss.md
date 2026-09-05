# Verify recovery after a VM power cut

Use a disposable Linux or macOS VM with a persistent disk and a VM monitor that
can stop power immediately. This check destroys the VM's running processes. Keep
the test VM separate from machines that hold real work.

1. Install the candidate Mesh binary in the VM. Set `MESH_STATE_DIR` and
   `MESH_CONFIG_DIR` to dedicated test directories on its persistent disk. Record
   the binary's checksum and the disk's filesystem and mount options.
2. Start `mesh local -- bash` and enable the generated Bash hook with
   `eval "$(mesh shell-init bash)"`. Set `HISTFILE` to a test file, enable history,
   and change into a directory whose name contains spaces.
3. Run `printf 'power-cut-checkpoint\n'`. Wait for the next prompt. Record the
   exact `MESH_SESSION_ID` outside the VM.
4. From a second guest terminal, run
   `mesh recovery-command SESSION -- /bin/true`. Its successful response
   acknowledges a durable checkpoint. Run `mesh logs SESSION --previous` and
   record the timestamp and marker outside the VM. Do not run a guest-wide sync.
5. Cut the VM's power immediately through its monitor. For a disposable QEMU
   process, terminate that process with SIGKILL. Do not use guest shutdown,
   `system_powerdown`, suspend, or a saved-memory snapshot.
6. Boot the same VM disk with no saved RAM. Verify that `mesh ls` shows the source
   as interrupted and `mesh logs SESSION --previous` contains the acknowledged
   marker and checkpoint time.
7. Run `mesh recover SESSION`. Confirm the shell opens in the saved directory,
   its ID differs from the source ID, and `history` contains the completed command.
   Confirm the old record and its previous output remain available.
8. From another guest terminal, retry recovery of the source. It must resolve the
   same replacement and refuse attachment takeover unless requested. Check that
   it does not start another worker.
9. Repeat with Zsh and its generated hook. Include disabled history and excluded
   commands. Confirm that excluded commands never appear in the history file.

Retain the guest filesystem details, binary checksum, checkpoint acknowledgement,
VM monitor power-cut time, recovered IDs, and history results with the release
verification. Repeat a cut before the next prompt to measure the unsaved window.
Shell history appends do not call fsync, so report any loss separately from the
acknowledged Mesh checkpoint. A process-kill integration test is not a substitute
for this procedure.
