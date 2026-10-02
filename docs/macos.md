# Run Mesh on macOS

## Quarantine

Releases are ad-hoc signed by the Go linker rather than with an Apple Developer
ID, so macOS quarantines the binary and Gatekeeper offers only to move it to
the trash. The cask clears that attribute on install. Homebrew removed
`--no-quarantine`, so a binary downloaded by hand needs it cleared by hand:

```bash
xattr -d com.apple.quarantine ./mesh
```

## File access

If macOS asks whether Mesh may access Desktop, Documents, or Downloads, you can
approve those folders individually in **System Settings > Privacy & Security >
Files & Folders**. To cover protected folders together, add Mesh in **Full Disk
Access** instead. This grants broad file access to Mesh and commands running in
its sessions. macOS requires you to make this approval manually.

Click **+**, then press **Cmd+Shift+G** in the file picker and enter the executable
path. For the daemon installed by `mesh daemon install` or `mesh add`, use
`~/.local/bin/mesh` on the Mac that hosts the sessions. For a Homebrew command,
run `command -v mesh` to find its path. Grant access to the executable running
the sessions; a grant to the Homebrew copy does not cover the separate daemon
copy. If macOS names your terminal app in the prompt, grant access to that app.

After granting access, restart the daemon and create a new session. For the
installed LaunchAgent, run this on the Mac:

```bash
launchctl kickstart -k "gui/$(id -u)/dev.shaulavo.mesh"
```

For an installation without a GUI login domain, use `user/$(id -u)` instead of
`gui/$(id -u)`. Existing detached workers survive a daemon restart, so reattaching
an old session does not restart its processes. For Mesh launched directly in a
terminal, quit and relaunch it and create a new session.

These approvals may need renewing after updates because current releases are
ad-hoc signed. Keeping approvals across releases requires a stable signing
identity. Apple's [file-access documentation](https://support.apple.com/guide/security/secddd1d86a6/web)
and [code-signing requirements](https://developer.apple.com/documentation/technotes/tn3127-inside-code-signing-requirements)
describe these controls.
