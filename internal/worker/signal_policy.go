package worker

import (
	"os"
	"os/signal"
	"syscall"
)

// HoldTerminationSignals keeps SIGINT, SIGHUP and SIGTERM from ending the
// worker. Call it first thing in a session-worker process.
//
// SIGINT and SIGHUP belong to a client's terminal, and a setsid'd worker has
// none. SIGTERM is held too, deliberately: mesh.service (KillMode=process) and
// the launchd agent (AbandonProcessGroup) never send it to a worker, but
// `systemctl --user kill mesh` and `killall mesh` do, with no SIGKILL behind
// it, while meaning to restart the daemon (invariant 3). A host shutdown still
// ends in SIGKILL, and the boot ID then reports the session as interrupted.
//
// The signals are caught, not ignored: an ignored disposition survives exec,
// and the session's command would start unable to see Ctrl-C. The channel is
// never read; a full channel drops the signal.
func HoldTerminationSignals() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM)
}
