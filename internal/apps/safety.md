Temporary app servers must bind their configured port only to `127.0.0.1` or
`::1`. Configure the server's host explicitly. A wildcard (`0.0.0.0`, `::`,
or `*`), LAN address, or Tailnet address can bypass Mesh's private-view gate.
Mesh checks existing listeners before launch and validates actual listeners
before admitting app traffic. Failure to inspect listeners is an error.

Port preflight releases its test sockets before launch; it does not reserve the
port. Startup must still check readiness and the actual listening addresses.
Linux uses the kernel TCP tables. macOS uses the system's numeric `netstat`
output. Neither check creates OS process or network isolation. An authorized
owner's setup and runtime commands retain Mesh's existing host privileges.

Uploads and setup require an absolute workload directory with at least 128 MiB
of filesystem space available. On Linux, paths under `/work` and `/data` require
those exact SSD mountpoints to be mounted. A symlink cannot silently redirect a
configured data-SSD path onto another filesystem. These checks use filesystem
free space; `/work`'s VDO logical capacity is not its physical SSD capacity.

On Linux, a listener belongs to an app when its owner UID matches Mesh's user
and its socket is held by a process in the app's kernel session or private Mesh
scope. A supervisor can move a server to another systemd scope while preserving
its kernel session. Children that use `setsid` remain attributable inside the
private Mesh scope. A server that leaves both groups requires a launcher that
keeps one of these identities.
